package events

import (
	"strings"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func hanEntities(entities []tbapi.MessageEntity) *[]bot.Entity {
	result := make([]bot.Entity, 0, len(entities))
	for _, e := range entities {
		result = append(result, bot.Entity{Type: e.Type, Offset: e.Offset, Length: e.Length})
	}
	return &result
}

func hanModerationReason(checks []spamcheck.Response, independent bool) string {
	reasons := make([]string, 0, len(checks))
	for _, c := range checks {
		if c.Name == "han script" && c.Spam {
			reasons = append(reasons, c.Details)
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	if !independent {
		return strings.Join(reasons, "\n")
	}
	for _, c := range checks {
		if !c.Spam || c.Name == "han script" {
			continue
		}
		name := c.Name
		switch name {
		case "external inline button":
			name = "внешняя кнопка со ссылкой"
		case "stopword", "stop words":
			name = "стоп-слово"
		case "burst":
			name = "повторяющиеся сообщения (burst)"
		}
		reasons = append(reasons, "Независимое основание модерации: "+name+" — "+c.Details)
	}
	return strings.Join(reasons, "\n")
}
