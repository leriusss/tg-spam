package bot

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestBotStartSpamIncidentExamples(t *testing.T) {
	examples := []string{
		"/start 5 @zopj5abot\n@rd5uojbot\n@bqof3ab8bot",
		"/start 69 @s4h41bot\n@uv08jabot\n@qzwgz45y9bot",
		"/start 9 @k9m46ys3mrbot\n@kyhlsbot\n@egsy3i06bot",
		"/start 92 @g9423549c29a5bot\n@jjrzqa6dhlbot\n@g9b3662f8981bbot",
		"/start 3 @g80f3552103a7bot\n@vq07ih6bot\n@v19ewnpgbot",
		"/start 66 @gaec5aee490dabot\n@gd14aa967578cbot\n@qyqjhca2hbot",
		"/start 1 @v19ewnpgbot\n@mcskkpbot\n@itpe825m2bot",
		"/start 484 @ufu3k81vbot\n@gd04ecf312586bot\n@f72qlgruc4bot",
		"/start 70 @c6mjqlwnbot\n@cpr8k4bot\n@hp2ge3bot",
		"/start 31 @blfpwjgbot\n@gd34ffccc60d1bot\n@ubhd7bot",
	}
	for _, text := range examples {
		t.Run(text, func(t *testing.T) {
			filter, d, msg := botStartFixture(t)
			msg.Text = text
			before := d.ApprovedUsers()
			response := filter.OnMessage(msg, false)
			require.True(t, response.Send)
			require.True(t, response.DeleteReplyTo)
			require.Equal(t, PermanentBanDuration, response.BanInterval)
			require.Equal(t, msg.ID, response.ReplyTo)
			require.Equal(t, msg.From.ID, response.User.ID)
			require.Equal(t, before, d.ApprovedUsers(), "a matched guard must not add ham/trust progress")
			found := false
			for _, check := range response.CheckResults {
				found = found || check.Name == "bot-start spam" && check.Spam
			}
			require.True(t, found)
		})
	}
}

func TestBotStartSpamOrdinaryMessages(t *testing.T) {
	for _, text := range []string{
		"", "Привет!", "Когда открывается Балтия?", "/start", "/start 123", "/start order_123",
		"/start 5 @first_bot", "/start 5 @first_bot @second_bot",
		"/start 5 @first_bot @FIRST_BOT @first_bot", "/start 5 @first_bot @FIRST_BOT @second_bot",
		"Посмотрите @first_bot @second_bot @third_bot", "Спасибо @first_bot @second_bot @third_bot",
		"Это спам: /start 5 @first_bot @second_bot @third_bot",
		"Как запустить /start 5 @first_bot @second_bot @third_bot?",
		"/start 5 @first_bot @second_bot @third_bot инструкция",
		"/start 5 @first_bot @second_bot @third_user", "/start 5 @first_bot @second_bot https://example.invalid",
		"/start five @first_bot @second_bot @third_bot", "/start -5 @first_bot @second_bot @third_bot",
		"/start 1.5 @first_bot @second_bot @third_bot", "/start@first_bot 5 @first_bot @second_bot @third_bot",
		"/report 5 @first_bot @second_bot @third_bot", "`/start 5 @first_bot @second_bot @third_bot`",
		"> /start 5 @first_bot @second_bot @third_bot", "/start 5 @ab\u043ebot @second_bot @third_bot",
		"/start 5 @bot @second_bot @third_bot", "/start 5 @1first_bot @second_bot @third_bot",
		"/start ٥ @first_bot @second_bot @third_bot",
	} {
		t.Run(text, func(t *testing.T) {
			filter, _, msg := botStartFixture(t)
			msg.Text = text
			response := filter.OnMessage(msg, true)
			require.False(t, response.Send)
			require.False(t, response.DeleteReplyTo)
		})
	}
}

func TestBotStartSpamBoundaries(t *testing.T) {
	for _, mode := range []string{"disabled", "manual", "administrator", "forward", "quote", "reply", "reply content only",
		"caption", "channel", "code", "pre", "blockquote", "expandable_blockquote", "system", "check only", "fresh user"} {
		t.Run(mode, func(t *testing.T) {
			filter, d, msg := botStartFixture(t)
			msg.Text = "/start 5 @first_bot @second_bot @third_bot"
			checkOnly := false
			switch mode {
			case "disabled":
				filter.params.BotStartSpamEnabled = false
			case "manual":
				require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "42", Source: approved.SourceManual}))
			case "administrator":
				msg.SkipBotStartSpam = true
			case "forward":
				msg.WithForward = true
			case "quote":
				msg.Quote = "пояснение"
			case "reply":
				msg.ReplyTo.Text = "пояснение"
			case "reply content only":
				msg.ReplyTo.Text, msg.Text = msg.Text, "Посмотрите, что прислали"
			case "caption":
				msg.Image = &Image{Caption: msg.Text}
			case "channel":
				msg.SenderChat.ID = -100
				checkOnly = true
			case "code", "pre", "blockquote", "expandable_blockquote":
				msg.Entities = &[]Entity{{Type: mode, Offset: 0, Length: len(msg.Text)}}
			case "system":
				msg.From.ID = 0
			case "check only":
				checkOnly = true
			case "fresh user":
				msg.From.ID = 99
			}
			before := d.ApprovedUsers()
			response := filter.OnMessage(msg, checkOnly)
			want := mode == "check only" || mode == "fresh user"
			require.Equal(t, want, response.Send)
			if want {
				require.True(t, response.DeleteReplyTo)
			}
			require.Equal(t, before, d.ApprovedUsers())
		})
	}
}

func TestBotStartSpamWhitespaceAndDistinctNames(t *testing.T) {
	require.True(t, matchesBotStartSpam(" \t/START 00042\r\n@First_bot\t@Second_bot\n@Third_bot @first_bot "))
	require.True(t, matchesBotStartSpam("/start 1 @aabot @bbbot @ccbot @ddbot"))
}

func botStartFixture(t *testing.T) (*SpamFilter, *tgspam.Detector, Message) {
	t.Helper()
	d := tgspam.NewDetector(tgspam.Config{FirstMessageOnly: true, FirstMessagesCount: 3, MinMsgLen: 1, MaxAllowedEmoji: -1})
	filter := NewSpamFilter(d, SpamConfig{BotStartSpamEnabled: true})
	msg := Message{ID: 1, ChatID: 123, From: User{ID: 42}, Text: "обычный текст"}
	for range 4 {
		filter.OnMessage(msg, false)
		msg.ID++
	}
	require.True(t, filter.IsApprovedUserInChat(42, 123))
	require.False(t, filter.IsExplicitTrustedUser(42))
	return filter, d, msg
}
