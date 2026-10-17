package tgspam_test

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func openLearningDB(t *testing.T, path string) (*engine.SQL, *storage.Samples, *tgspam.Detector, *bot.SpamFilter) {
	t.Helper()
	db, err := engine.NewSqlite(path, "learning-test")
	require.NoError(t, err)
	store, err := storage.NewSamples(context.Background(), db)
	require.NoError(t, err)
	for text, label := range map[string]storage.SampleType{
		"win lottery prize": storage.SampleTypeSpam, "hello peaceful world": storage.SampleTypeHam,
		"normal friendly conversation": storage.SampleTypeHam,
	} {
		require.NoError(t, store.Add(context.Background(), label, storage.SampleOriginPreset, text))
	}
	d := tgspam.NewDetector(tgspam.Config{MaxAllowedEmoji: -1, SimilarityThreshold: .5, MinSpamProbability: 45})
	spam, ham, err := store.Snapshot(context.Background())
	require.NoError(t, err)
	_, err = d.LoadSamples(strings.NewReader("есть"), []io.Reader{strings.NewReader(strings.Join(spam, "\n"))},
		[]io.Reader{strings.NewReader(strings.Join(ham, "\n"))})
	require.NoError(t, err)
	d.WithSpamUpdater(storage.NewSampleUpdater(store, storage.SampleTypeSpam, 0))
	d.WithHamUpdater(storage.NewSampleUpdater(store, storage.SampleTypeHam, 0))
	return db, store, d, bot.NewSpamFilter(d, bot.SpamConfig{})
}

func learningChecks(d *tgspam.Detector) [][]spamcheck.Response {
	result := make([][]spamcheck.Response, 0, 4)
	for _, text := range []string{"Есть проба 0.5г", "Есть проба 0.7г", "Проба золота 0.5г", "hello peaceful world"} {
		_, checks := d.Check(spamcheck.Request{Msg: text, AuthoredText: new(string), CheckOnly: true})
		result = append(result, checks)
	}
	return result
}

func TestLearningSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "learning.sqlite")
	db, store, d, filter := openLearningDB(t, path)
	t.Cleanup(func() { _ = db.Close() })
	for _, step := range []struct {
		name   string
		spam   bool
		remove bool
	}{
		{"add", true, false}, {"repeat", true, false}, {"ham", false, false}, {"repeat ham", false, false},
		{"spam again", true, false}, {"delete", true, true}, {"delete absent", true, true},
		{"add ham", false, false}, {"delete ham", false, true}, {"delete ham absent", false, true},
	} {
		t.Run(step.name, func(t *testing.T) {
			var err error
			if step.remove {
				if step.spam {
					err = d.RemoveSpam("Есть проба 0.5г")
				} else {
					err = d.RemoveHam("Есть проба 0.5г")
				}
			} else if step.spam {
				err = filter.UpdateSpam("Есть проба\n0.5г")
			} else {
				err = filter.UpdateHam("Есть проба\n0.5г")
			}
			require.NoError(t, err)
			before := learningChecks(d)
			spam, ham, err := store.Snapshot(context.Background())
			require.NoError(t, err)
			require.NoError(t, db.Close())
			db, store, d, filter = openLearningDB(t, path)
			afterSpam, afterHam, err := store.Snapshot(context.Background())
			require.NoError(t, err)
			require.Equal(t, spam, afterSpam)
			require.Equal(t, ham, afterHam)
			require.Equal(t, before, learningChecks(d))
		})
	}
}

func TestLearningSQLiteConcurrentReplayAndChecks(t *testing.T) {
	db, store, d, filter := openLearningDB(t, filepath.Join(t.TempDir(), "learning.sqlite"))
	defer db.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { errs <- filter.UpdateSpam("Есть проба 0.5г"); learningChecks(d) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	spam, _, err := store.Snapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, spam, 2)
	before := learningChecks(d)
	require.NoError(t, filter.UpdateSpam("Есть проба 0.5г"))
	require.Equal(t, before, learningChecks(d))
	var firstID, secondID int64
	require.NoError(t, db.Get(&firstID, "SELECT id FROM samples WHERE message=?", "Есть проба 0.5г"))
	require.NoError(t, filter.UpdateSpam("Есть проба 0.5г"))
	require.NoError(t, db.Get(&secondID, "SELECT id FROM samples WHERE message=?", "Есть проба 0.5г"))
	require.Equal(t, firstID, secondID)
}

func TestLearningSQLiteSingleConnectionReload(t *testing.T) {
	db, store, d, _ := openLearningDB(t, filepath.Join(t.TempDir(), "learning.sqlite"))
	defer db.Close()
	dict, err := storage.NewDictionary(context.Background(), db)
	require.NoError(t, err)
	filter := bot.NewSpamFilter(d, bot.SpamConfig{SamplesStore: store, DictStore: dict})
	db.SetMaxOpenConns(1)
	_, err = d.LoadSamples(strings.NewReader(""), nil, nil)
	require.NoError(t, err)
	require.NoError(t, filter.UpdateSpam("Есть проба 0.5г"))
	before := learningChecks(d)
	require.NoError(t, filter.ReloadSamples())
	require.Equal(t, before, learningChecks(d))
}
