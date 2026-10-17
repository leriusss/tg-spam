package events

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func eventLearningFilter(t *testing.T) (*bot.SpamFilter, *storage.Samples) {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "learning.sqlite"), "events")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.NewSamples(context.Background(), db)
	require.NoError(t, err)
	for text, label := range map[string]storage.SampleType{
		"win lottery prize": storage.SampleTypeSpam, "hello peaceful world": storage.SampleTypeHam,
		"normal friendly conversation": storage.SampleTypeHam,
	} {
		require.NoError(t, store.Add(context.Background(), label, storage.SampleOriginPreset, text))
	}
	return reloadEventLearning(t, store), store
}

func reloadEventLearning(t *testing.T, store *storage.Samples) *bot.SpamFilter {
	t.Helper()
	d := tgspam.NewDetector(tgspam.Config{MaxAllowedEmoji: -1, SimilarityThreshold: .5, MinSpamProbability: 45})
	spam, ham, err := store.Snapshot(context.Background())
	require.NoError(t, err)
	_, err = d.LoadSamples(strings.NewReader(""), []io.Reader{strings.NewReader(strings.Join(spam, "\n"))},
		[]io.Reader{strings.NewReader(strings.Join(ham, "\n"))})
	require.NoError(t, err)
	d.WithSpamUpdater(storage.NewSampleUpdater(store, storage.SampleTypeSpam, 0))
	d.WithHamUpdater(storage.NewSampleUpdater(store, storage.SampleTypeHam, 0))
	return bot.NewSpamFilter(d, bot.SpamConfig{})
}

func eventLearningChecks(filter *bot.SpamFilter, text string) []spamcheck.Response {
	_, checks := filter.Check(spamcheck.Request{Msg: text, AuthoredText: new(string), CheckOnly: true})
	return checks
}

func TestLearningDirectSpamCommandReplay(t *testing.T) {
	filter, store := eventLearningFilter(t)
	a := &admin{bot: filter, primChatID: 123, adminChatID: 456, superUsers: SuperUsers{"operator"}, tbAPI: &mocks.TbAPIMock{
		RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) { return apiOK(true), nil },
		SendFunc:    func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil },
	}}
	u := tbapi.Update{Message: &tbapi.Message{MessageID: 700, Chat: tbapi.Chat{ID: 123},
		From: &tbapi.User{ID: 11, UserName: "operator"}, Text: "/spam",
		ReplyToMessage: &tbapi.Message{MessageID: 701, Date: int(time.Now().Unix()), Chat: tbapi.Chat{ID: 123},
			From: &tbapi.User{ID: 501, UserName: "subject"}, Text: "win additional lottery prize"}}}
	require.NoError(t, a.DirectSpamReport(u))
	before := eventLearningChecks(filter, u.Message.ReplyToMessage.Text)
	require.NoError(t, a.DirectSpamReport(u))
	require.Equal(t, before, eventLearningChecks(filter, u.Message.ReplyToMessage.Text))
	require.Equal(t, before, eventLearningChecks(reloadEventLearning(t, store), u.Message.ReplyToMessage.Text))
}

func TestLearningRecoveryCallbackReplayAndRelabel(t *testing.T) {
	a, _, card := cardFixture(t, "detector")
	filter, store := eventLearningFilter(t)
	a.bot = filter
	require.NoError(t, a.recoveryCardCallback(cardQuery(card, "M+", "learning-approve")))
	before := eventLearningChecks(filter, card.Text)
	require.NoError(t, a.recoveryCardCallback(cardQuery(card, "M+", "learning-approve")))
	require.Equal(t, before, eventLearningChecks(filter, card.Text))
	require.Equal(t, before, eventLearningChecks(reloadEventLearning(t, store), card.Text))
	require.NoError(t, a.recoveryCardCallback(cardQuery(card, "MU", "learning-unban")))
	require.Equal(t, eventLearningChecks(reloadEventLearning(t, store), card.Text), eventLearningChecks(filter, card.Text))
	_, ham, err := store.Snapshot(context.Background())
	require.NoError(t, err)
	require.Contains(t, ham, strings.ReplaceAll(card.Text, "\n", " "))
}
