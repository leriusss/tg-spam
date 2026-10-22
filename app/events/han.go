package events

import (
	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/bot"
)

func hanEntities(entities []tbapi.MessageEntity) *[]bot.Entity {
	result := make([]bot.Entity, 0, len(entities))
	for _, e := range entities {
		result = append(result, bot.Entity{Type: e.Type, Offset: e.Offset, Length: e.Length})
	}
	return &result
}
