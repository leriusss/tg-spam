package events

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

type deleteCandidateLocator interface {
	DeleteCandidates(context.Context, string, *storage.MediaLocatorKey, int64, int) ([]storage.MsgMeta, error)
}

func (a *admin) originalDeleteCandidates(u tbapi.Update, locator deleteCandidateLocator) error {
	m := u.Message
	reply := func(text string) error {
		msg := tbapi.NewMessage(a.adminChatID, text)
		msg.ReplyParameters = tbapi.ReplyParameters{MessageID: m.MessageID}
		if _, err := a.tbAPI.Send(msg); err != nil {
			log.Printf("[WARN] original delete feedback unavailable")
		}
		return nil
	}
	if m.Chat.ID != a.adminChatID ||
		m.From == nil ||
		m.From.IsBot ||
		m.From.ID <= 0 ||
		sharedTelegramUser(m.From.ID) ||
		!a.superUsers.IsSuper("",
			m.From.ID) {
		return reply("Выбор оригинала доступен только уполномоченному пользователю с числовым ID. Действия не выполнялись.")
	}
	if a.recovery == nil || a.dry || a.trainingMode {
		return reply("Удаление недоступно: необходим recovery journal и рабочий режим без dry/training. " +
			"Действия и обучение не выполнялись.")
	}
	text := transform(m).Text
	var media *storage.MediaLocatorKey
	if strings.TrimSpace(text) == "" {
		key, status := extractMediaLocatorKey(m)
		if status != mediaLocatorExtracted {
			return reply("Не удалось безопасно сопоставить эту пересылку. Поддерживаются текст, подпись и " +
				"одиночное фото, видео, документ или анимация. Действия не выполнялись.")
		}
		media = &key
	}
	rows, err := locator.DeleteCandidates(context.Background(), text, media, a.primChatID, 21)
	if err != nil {
		log.Printf("[WARN] original delete lookup unavailable")
		return reply("Не удалось прочитать сохранённые оригиналы. Действия не выполнялись; попробуйте позже.")
	}
	if len(rows) == 0 {
		return reply("Сохранённый оригинал в защищаемом чате не найден. Бан, удаление и обучение не выполнялись.")
	}
	if len(rows) > 20 {
		return reply("Найдено более 20 совпадений. Автоматический выбор небезопасен. Уточните оригинал " +
			"ответом на него в исходном чате; действия не выполнялись.")
	}
	created := 0
	for _, row := range rows {
		if row.ChatID != a.primChatID || row.MsgID <= 0 {
			return reply("Сохранённые данные не прошли проверку чата и оригинала. Действия не выполнялись.")
		}
		kind := "message"
		id := int64(0)
		name := ""
		if row.UserID > 0 && !sharedTelegramUser(row.UserID) {
			kind = "user"
			id = row.UserID
			name = row.UserName
		}
		if row.UserID < 0 {
			kind = "sender_chat"
			id = row.UserID
			name = row.UserName
		}
		if id != 0 && (a.superUsers.IsSuper(name, id) || a.targetExplicitlyApproved(id)) {
			continue
		}
		p := moderation.Plan{DeleteOnly: true,
			Key: fmt.Sprintf("delete:%d:%d",
				row.ChatID,
				row.MsgID),
			ChatID:       row.ChatID,
			MessageID:    row.MsgID,
			IdentityKind: kind,
			IdentityID:   id,
			UserName:     name,
			Destination:  a.adminChatID,
			RequesterID:  m.From.ID,
			Origin:       "delete_request",
			Card:         true,
			SourceText:   text}
		p.Actions = []moderation.Action{{Kind: "original_delete",
			ChatID:       row.ChatID,
			MessageID:    row.MsgID,
			IdentityKind: kind,
			IdentityID:   id,
			MessageDate:  locatorMessageDate(a.locator, row.ChatID, row.MsgID),
			State:        moderation.Manual,
			Code:         "delete_pending_confirmation"}}
		_, _, fresh, e := a.recovery.journal.Accept(context.Background(), p, time.Now())
		if e != nil {
			log.Printf("[WARN] original delete request admission unavailable")
			return reply("Не все карточки удалось сохранить: достигнут лимит или журнал недоступен. " +
				"Сохранённые карточки требуют отдельного подтверждения. Модерация и обучение не выполнялись.")
		}
		if fresh {
			created++
		}
	}
	if created == 0 {
		return reply("Новых карточек нет: оригиналы уже зарегистрированы либо защищены правилами доверия и " +
			"полномочий. Для нового подтверждения нажмите «Создать новый запрос» в карточке; повторных действий не создано.")
	}
	return reply(fmt.Sprintf("Найдено совпадений: %d. Подготовлено карточек: %d. Выберите нужный оригинал по чату "+
		"и номеру сообщения и нажмите «Удалить сообщение» в его карточке (15 минут). "+
		"Совпадение текста не разрешает бан. Обучение и очистка не выполняются.",
		len(rows),
		created))
}

func deleteOnlyKeyboard(c moderation.Card, allowed bool) tbapi.InlineKeyboardMarkup {
	rows := [][]tbapi.InlineKeyboardButton{tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Подробнее", "M?:"+c.Ref))}
	if allowed && c.RequestState == "pending" {
		rows = append(rows,
			tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Удалить сообщение",
				"MD:"+c.Ref),
				tbapi.NewInlineKeyboardButtonData("Отклонить",
					"MX:"+c.Ref)))
	}
	renewable := true
	for _, op := range c.Actions {
		if op.Kind == "original_delete" && (op.State == moderation.Pending || op.State == moderation.Running ||
			op.State == moderation.Confirmed || op.State == moderation.Absent || op.Uncertain) {
			renewable = false
		}
	}
	if allowed && renewable {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Создать новый запрос", "MN:"+c.Ref)))
	}
	if c.RequestState == "expired" {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Закрыть запрос", "MX:"+c.Ref)))
	}
	return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func originalDeleteCardText(c moderation.Card, chatName string) string {
	title := "Выбор оригинала"
	if chatName != "" {
		title += " — " + truncateRunes(chatName, 100)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "🗑 %s\nЧат: %d\nОригинал: %d\n«%s»\n\nТолько удаление этого сообщения. Бан, очистка и "+
		"обучение не разрешены.\n",
		html.EscapeString(title),
		c.ChatID,
		c.MessageID,
		html.EscapeString(truncateRunes(inertSpamText(c.Text),
			650)))
	switch c.RequestState {
	case "pending":
		text.WriteString("Подтверждение ожидается до " + cardDate(c.RequestExpires/1000) + ".\n")
	case "expired":
		text.WriteString("Срок подтверждения истёк.\n")
	case "dismiss":
		text.WriteString("Запрос отклонён.\n")
	}
	for _, op := range c.Actions {
		if op.Kind == "original_delete" {
			if op.Code == "delete_pending_confirmation" && c.RequestState == "pending" {
				text.WriteString("Удаление ещё не разрешено.\n")
			} else {
				text.WriteString(compactAction(op) + "\n")
			}
			if op.MessageDate > 0 {
				text.WriteString("Дата оригинала: " + cardDate(op.MessageDate) + "\n")
			} else {
				text.WriteString("Дата оригинала неизвестна.\n")
			}
			if op.Deadline > 0 {
				text.WriteString("Срок удаления: " + cardDate(op.Deadline/1000) + "\n")
			}
			if op.Uncertain {
				text.WriteString("Результат прежней попытки не установлен; требуется ручная проверка.\n")
			}
		}
	}
	return text.String()
}

func (a *admin) originalDeleteCallback(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card, verb string) error {
	if q.From.ID <= 0 ||
		q.From.IsBot ||
		sharedTelegramUser(q.From.ID) || !a.superUsers.IsSuper("", q.From.ID) {
		return a.answerRecoveryCallback(ctx, q, "Подтвердить может только создавший запрос уполномоченный пользователь с числовым ID")
	}
	if verb == "M?" || verb == "MB" {
		return a.recoveryCardDisplay(ctx, q, c)
	}
	if verb != "MN" && q.From.ID != c.RequesterID {
		return a.answerRecoveryCallback(ctx, q, "Подтвердить может создавший запрос оператор. Для себя нажмите «Создать новый запрос»")
	}
	if verb != "MD" && verb != "MX" && verb != "MN" {
		return a.answerRecoveryCallback(ctx, q, "Эта карточка разрешает только удаление выбранного оригинала или отказ")
	}
	if a.dry || a.trainingMode {
		return a.answerRecoveryCallback(ctx, q, "Режим dry/training: удаление не разрешено")
	}
	target := moderation.Action{Kind: "original_delete",
		ChatID:       c.ChatID,
		MessageID:    c.MessageID,
		IdentityKind: c.IdentityKind,
		IdentityID:   c.IdentityID}
	if c.ChatID != a.primChatID ||
		(a.recovery.gate != nil &&
			!a.recovery.gate(target)) ||
		(c.IdentityID != 0 &&
			(a.superUsers.IsSuper(c.UserName,
				c.IdentityID) ||
				a.targetExplicitlyApproved(c.IdentityID))) {
		return a.answerRecoveryCallback(ctx, q, "Текущие правила безопасности не разрешают удаление этой цели")
	}
	if verb == "MN" {
		fresh, err := a.recovery.journal.RenewDelete(ctx, c.Ref, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, q.ID, time.Now())
		if err != nil {
			return a.answerRecoveryCallback(ctx, q, "Новый запрос не создан: проверьте актуальность карточки и состояние удаления")
		}
		if !fresh {
			return a.answerRecoveryCallback(ctx, q, "Новый запрос не нужен: действует ваш запрос либо "+
				"удаление уже выполняется или завершено")
		}
		return a.answerRecoveryCallback(ctx, q, "Новый запрос сохранён. Подтвердите удаление в новой карточке; "+
			"срок самого удаления не продлён")
	}
	fresh, err := a.recovery.journal.DecideDelete(ctx,
		c.Ref,
		q.Message.Chat.ID,
		q.Message.MessageID,
		q.From.ID,
		verb == "MD",
		time.Now())
	if err != nil {
		return a.answerRecoveryCallback(ctx,
			q,
			"Не удалось подтвердить запрос: проверьте срок, карточку и полномочия. Действия не добавлены")
	}
	if !fresh {
		return a.answerRecoveryCallback(ctx, q, "Запрос уже обработан или удаление выполняется. Повторных действий не добавлено")
	}
	return a.answerRecoveryCallback(ctx,
		q,
		"Решение сохранено. Фактический результат удаления появится в карточке; бан и "+
			"обучение не выполняются")
}
