package tgspam

import (
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

type learningStore struct {
	rows             map[string]spamClass
	saveErr, readErr error
}

type learningUpdater struct {
	store *learningStore
	label spamClass
}

func (u learningUpdater) Append(msg string) error {
	if u.store.saveErr != nil {
		return u.store.saveErr
	}
	u.store.rows[msg] = u.label
	return nil
}

func (u learningUpdater) Remove(msg string) error {
	if u.store.saveErr != nil {
		return u.store.saveErr
	}
	delete(u.store.rows, msg)
	return nil
}

func (u learningUpdater) Reader() (io.ReadCloser, error) {
	spam, ham, err := u.Snapshot()
	if u.label == ClassSpam {
		return io.NopCloser(strings.NewReader(strings.Join(spam, "\n"))), err
	}
	return io.NopCloser(strings.NewReader(strings.Join(ham, "\n"))), err
}

func (u learningUpdater) Snapshot() (spam, ham []string, err error) {
	if u.store.readErr != nil {
		return nil, nil, u.store.readErr
	}
	for text, label := range u.store.rows {
		if label == ClassSpam {
			spam = append(spam, text)
		} else {
			ham = append(ham, text)
		}
	}
	sort.Strings(spam)
	sort.Strings(ham)
	return spam, ham, nil
}

func learningDetector(t testing.TB, store *learningStore) *Detector {
	t.Helper()
	d := NewDetector(Config{MaxAllowedEmoji: -1, SimilarityThreshold: .5, MinSpamProbability: 45})
	u := learningUpdater{store, ClassSpam}
	spam, ham, err := u.Snapshot()
	require.NoError(t, err)
	_, err = d.LoadSamples(strings.NewReader("есть"), []io.Reader{strings.NewReader(strings.Join(spam, "\n"))},
		[]io.Reader{strings.NewReader(strings.Join(ham, "\n"))})
	require.NoError(t, err)
	d.WithSpamUpdater(u)
	d.WithHamUpdater(learningUpdater{store, ClassHam})
	return d
}

func TestLearningMatchesPersistedSnapshot(t *testing.T) {
	store := &learningStore{rows: map[string]spamClass{
		"win lottery prize": ClassSpam, "hello peaceful world": ClassHam, "normal friendly conversation": ClassHam,
	}}
	d := learningDetector(t, store)
	const text = "Есть проба 0.5г"
	for _, step := range []struct {
		name  string
		apply func(string) error
	}{
		{"add", d.UpdateSpam}, {"repeat", d.UpdateSpam}, {"ham", d.UpdateHam}, {"repeat ham", d.UpdateHam},
		{"spam again", d.UpdateSpam}, {"delete", d.RemoveSpam}, {"delete absent", d.RemoveSpam},
	} {
		t.Run(step.name, func(t *testing.T) {
			require.NoError(t, step.apply(text))
			fresh := learningDetector(t, store)
			require.Equal(t, fresh.classifier, d.classifier)
			require.Equal(t, fresh.tokenizedSpam, d.tokenizedSpam)
			for _, probe := range []string{text, "Есть проба 0.7г", "Проба золота 0.5г", "hello peaceful world"} {
				req := spamcheck.Request{Msg: probe, AuthoredText: new(string), CheckOnly: true}
				want, wc := fresh.Check(req)
				got, gc := d.Check(req)
				require.Equal(t, want, got)
				require.Equal(t, wc, gc)
			}
		})
	}
}

func TestLearningConcurrentReplay(t *testing.T) {
	store := &learningStore{rows: map[string]spamClass{"hello peaceful world": ClassHam}}
	d := learningDetector(t, store)
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			errCh <- d.UpdateSpam("win lottery prize")
			d.Check(spamcheck.Request{Msg: "win lottery prize", CheckOnly: true})
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	fresh := learningDetector(t, store)
	require.Equal(t, fresh.classifier, d.classifier)
	require.Equal(t, fresh.tokenizedSpam, d.tokenizedSpam)
}

func TestLearningSaveFailure(t *testing.T) {
	store := &learningStore{rows: map[string]spamClass{"hello peaceful world": ClassHam}}
	d := learningDetector(t, store)
	before := d.classifier
	store.saveErr = errors.New("disk failure")
	require.ErrorContains(t, d.UpdateSpam("win lottery prize"), "disk failure")
	require.Equal(t, before, d.classifier)
	require.Len(t, store.rows, 1)
}

func TestLearningSnapshotFailureRecovery(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "remove"}[remove], func(t *testing.T) {
			store := &learningStore{rows: map[string]spamClass{"hello peaceful world": ClassHam, "win lottery prize": ClassSpam}}
			d := learningDetector(t, store)
			before := d.classifier
			store.readErr = errors.New("snapshot unavailable")
			var err error
			if remove {
				err = d.RemoveSpam("win lottery prize")
			} else {
				err = d.UpdateSpam("additional lottery prize")
			}
			require.ErrorContains(t, err, "model unavailable")
			require.Equal(t, before, d.classifier, "failed reconstruction must not publish a partial model")
			_, checks := d.Check(spamcheck.Request{Msg: "hello peaceful world", CheckOnly: true})
			require.Len(t, checks, 1)
			require.Error(t, checks[0].Error)
			require.Equal(t, "sample model unavailable", checks[0].Name)
			store.readErr = nil
			d.Check(spamcheck.Request{Msg: "hello peaceful world", CheckOnly: true})
			require.NoError(t, d.samplesErr)
			fresh := learningDetector(t, store)
			require.Equal(t, fresh.classifier, d.classifier)
			require.Equal(t, fresh.tokenizedSpam, d.tokenizedSpam)
		})
	}
}

func TestLearningInvalidModelInput(t *testing.T) {
	store := &learningStore{rows: map[string]spamClass{"hello peaceful world": ClassHam}}
	d := learningDetector(t, store)
	before := d.classifier
	bad := strings.Repeat("a", 70*1024)
	require.ErrorContains(t, d.UpdateSpam(bad), "model unavailable")
	require.Equal(t, before, d.classifier)
	require.Error(t, d.samplesErr)
	delete(store.rows, bad)
	d.Check(spamcheck.Request{Msg: "hello peaceful world", CheckOnly: true})
	require.NoError(t, d.samplesErr)
	require.Equal(t, before, d.classifier)
}

func TestLearningChecksSeeWholeModels(t *testing.T) {
	store := &learningStore{rows: map[string]spamClass{"hello peaceful world": ClassHam, "normal friendly conversation": ClassHam}}
	d := learningDetector(t, store)
	text := "win lottery prize"
	req := spamcheck.Request{Msg: text, CheckOnly: true}
	require.NoError(t, d.UpdateSpam(text))
	_, spam := d.Check(req)
	require.NoError(t, d.UpdateHam(text))
	_, ham := d.Check(req)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	wg.Go(func() {
		for range 20 {
			errs <- d.UpdateSpam(text)
			errs <- d.UpdateHam(text)
		}
	})
	wg.Go(func() {
		for range 100 {
			_, checks := d.Check(req)
			if !reflect.DeepEqual(checks, spam) && !reflect.DeepEqual(checks, ham) {
				errs <- errors.New("partially published model")
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
