package bot

import (
	"regexp"
	"strings"
)

var (
	botStartNumber  = regexp.MustCompile(`^\d+$`)
	botStartMention = regexp.MustCompile(`(?i)^@[a-z][a-z0-9_]{1,28}bot$`)
)

func (s *SpamFilter) botStartSpam(msg Message, userID string) bool {
	if !s.params.BotStartSpamEnabled || msg.SkipBotStartSpam || msg.SenderChat.ID != 0 ||
		msg.WithForward || msg.Quote != "" || msg.ReplyTo.Text != "" || msg.Image != nil ||
		msg.WithVideo || msg.WithVideoNote || msg.WithAudio || msg.WithContact || msg.WithGiveaway ||
		msg.WithKeyboard || msg.WithExternalLinkButton {
		return false
	}
	if msg.Entities != nil {
		for _, entity := range *msg.Entities {
			switch entity.Type {
			case "code", "pre", "blockquote", "expandable_blockquote":
				return false
			}
		}
	}
	if !matchesBotStartSpam(msg.Text) {
		return false
	}
	return !s.Detector.IsExplicitTrustedUser(userID)
}

func matchesBotStartSpam(text string) bool {
	fields := strings.Fields(text)
	if len(fields) < 5 || !strings.EqualFold(fields[0], "/start") || !botStartNumber.MatchString(fields[1]) {
		return false
	}
	mentions := make(map[string]struct{}, len(fields)-2)
	for _, field := range fields[2:] {
		if !botStartMention.MatchString(field) {
			return false
		}
		mentions[strings.ToLower(field)] = struct{}{}
	}
	return len(mentions) >= 3
}
