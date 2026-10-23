package events

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

// Exercise the real report handler, detector and SQLite adapter together. Only
// Telegram is mocked; assertions inspect the actual reports/progress tables.
func TestDirectUserReportAutoTrustProgress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		chat       int64
		senderChat bool
		manual     bool
		duplicate  bool
		allowed    bool
	}{
		{name: "at threshold", count: 3, chat: -100, allowed: true},
		{name: "below threshold", count: 2, chat: -100},
		{name: "another chat", count: 3, chat: -200},
		{name: "SenderChat is not reporter", count: 3, chat: -100, senderChat: true},
		{name: "manual trust", manual: true, allowed: true},
		{name: "duplicates do not accelerate", count: 2, chat: -100, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "report.sqlite")
			var db *engine.SQL
			var filter *bot.SpamFilter
			var users *storage.ApprovedUsers
			var reports *storage.Reports
			open := func() {
				var err error
				db, err = engine.NewSqlite(path, "instance")
				require.NoError(t, err)
				users, err = storage.NewApprovedUsers(ctx, db)
				require.NoError(t, err)
				reports, err = storage.NewReports(ctx, db)
				require.NoError(t, err)
				d := tgspam.NewDetector(tgspam.Config{FirstMessagesCount: 3, MinMsgLen: 5, MaxAllowedEmoji: -1})
				_, err = d.LoadSamples(strings.NewReader(""), []io.Reader{strings.NewReader("win lottery prize")},
					[]io.Reader{strings.NewReader("normal friendly conversation")})
				require.NoError(t, err)
				_, err = d.WithUserStorage(users)
				require.NoError(t, err)
				filter = bot.NewSpamFilter(d, bot.SpamConfig{})
			}
			open()
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if tc.manual {
				require.NoError(t, filter.AddApprovedUser(42, "manual"))
			}
			feed := func(id int) {
				msg := bot.Message{ID: id, ChatID: tc.chat, Text: "normal friendly conversation", From: bot.User{ID: 42}}
				if tc.senderChat {
					msg.SenderChat = bot.SenderChat{ID: -42, UserName: "channel"}
				}
				resp := filter.OnMessage(msg, false)
				require.Zero(t, resp.BanInterval)
				require.False(t, resp.DeleteReplyTo)
			}
			for id := 1; id <= tc.count; id++ {
				feed(id)
				if tc.duplicate {
					feed(id)
					feed(id)
				}
			}
			attempt := func(phase int, allowed bool) {
				api := &mocks.TbAPIMock{RequestFunc: func(req tbapi.Chattable) (*tbapi.APIResponse, error) {
					del, ok := req.(tbapi.DeleteMessageConfig)
					require.True(t, ok, "only the report command can be deleted")
					require.Equal(t, 700+phase, del.MessageID)
					require.EqualValues(t, -100, del.ChatID)
					return &tbapi.APIResponse{Ok: true}, nil
				}}
				r := &userReports{ReportConfig: ReportConfig{Storage: reports, Threshold: 100}, bot: filter, tbAPI: api, primChatID: -100}
				u := tbapi.Update{Message: &tbapi.Message{MessageID: 700 + phase, Chat: tbapi.Chat{ID: -100}, From: &tbapi.User{ID: 42}, Text: "/report",
					ReplyToMessage: &tbapi.Message{MessageID: 900 + phase, Chat: tbapi.Chat{ID: -100}, From: &tbapi.User{ID: 99}, Text: "reported text"}}}
				before, err := users.Read(ctx)
				require.NoError(t, err)
				err = r.DirectUserReport(ctx, u)
				if allowed {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "not in approved list")
				}
				saved, err := reports.GetByMessage(ctx, 900+phase, -100)
				require.NoError(t, err)
				if allowed {
					require.Len(t, saved, 1)
					require.EqualValues(t, 42, saved[0].ReporterUserID)
				} else {
					require.Empty(t, saved)
				}
				after, err := users.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after, "report eligibility must not inflate progress")
			}
			attempt(0, tc.allowed)
			require.NoError(t, db.Close())
			open()
			if tc.duplicate {
				for id := 1; id <= tc.count; id++ {
					feed(id)
				}
			}
			attempt(1, tc.allowed)
			if tc.duplicate {
				feed(3)
				attempt(2, true)
			}
			rows, err := users.Read(ctx)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			if !tc.manual {
				require.NotEqual(t, approved.SourceManual, rows[0].Source)
			}
		})
	}
}
