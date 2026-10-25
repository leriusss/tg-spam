// Package events provide event handlers for telegram bot and all the high-level event handlers.
// It parses messages, sends them to the spam detector and handles the results. It can also ban users
// and send messages to the admin.
//
// In addition to that, it provides support for admin chat handling allowing to unban users via the web service and
// update the list of spam samples.
package events

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/hashicorp/go-multierror"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

// TelegramListener listens to tg update, forward to bots and send back responses
// Not thread safe
type TelegramListener struct {
	RecoveryJournal         *moderation.Journal // optional durable stage-2 authority
	TbAPI                   TbAPI               // telegram bot API
	SpamLogger              SpamLogger          // logger to save spam to files and db
	Bot                     Bot                 // bot to handle messages
	BotUsername             string              // telegram bot username (without "@" prefix)
	Group                   string              // can be int64 or public group username (without "@" prefix)
	AdminGroup              string              // can be int64 or public group username (without "@" prefix)
	IdleDuration            time.Duration       // idle timeout to send "idle" message to bots
	SuperUsers              SuperUsers          // list of superusers, can ban and report spam, can't be banned
	TestingIDs              []int64             // list of chat IDs to test the bot
	StartupMsg              string              // message to send on startup to the primary chat
	WarnMsg                 string              // message to send on warning
	NoSpamReply             bool                // do not reply on spam messages in the primary chat
	SuppressJoinMessage     bool                // delete join message when kick out user
	DeleteJoinMessages      bool                // delete join messages immediately
	DeleteLeaveMessages     bool                // delete leave messages immediately
	TrainingMode            bool                // do not ban users, just report and train spam detector
	SoftBanMode             bool                // do not ban users, but restrict their actions
	Locator                 Locator             // message locator to get info about messages
	ReportConfig            ReportConfig        // user spam reporting configuration
	DisableAdminSpamForward bool                // disable forwarding spam reports to admin chat support
	Dry                     bool                // dry run, do not ban or send messages
	AggressiveCleanup       bool                // delete all messages from user when banned via /spam command
	AggressiveCleanupLimit  int                 // max messages to delete in aggressive cleanup mode

	senderFeedbackAt time.Time
	adminHandler     *admin
	reportsHandler   *userReports
	chatID           int64
	adminChatID      int64

	msgs struct {
		once sync.Once
		ch   chan bot.Response
	}
}

// Do process all events, blocked call
func (l *TelegramListener) Do(ctx context.Context) error {
	log.Printf("[INFO] start telegram listener for %q", l.Group)

	if l.TrainingMode {
		log.Printf("[WARN] training mode, no bans")
	}

	if l.SoftBanMode {
		log.Printf("[INFO] soft ban mode, no bans but restrictions")
	}

	// get chat ID for the group we are monitoring
	var getChatErr error
	if l.chatID, getChatErr = l.getChatID(l.Group); getChatErr != nil {
		return fmt.Errorf("failed to get chat ID for group %q: %w", l.Group, getChatErr)
	}
	log.Printf("[INFO] primary chat ID: %d", l.chatID)

	if err := l.updateSupers(); err != nil {
		log.Printf("[WARN] failed to update superusers: %v", err)
	}

	if l.AdminGroup != "" {
		// get chat ID for the admin group
		if l.adminChatID, getChatErr = l.getChatID(l.AdminGroup); getChatErr != nil {
			return fmt.Errorf("failed to get chat ID for admin group %q: %w", l.AdminGroup, getChatErr)
		}
		log.Printf("[INFO] admin chat ID: %d", l.adminChatID)
	}

	l.msgs.once.Do(func() {
		l.msgs.ch = make(chan bot.Response, 100)
		if l.IdleDuration == 0 {
			l.IdleDuration = 30 * time.Second
		}
	})

	// send startup message if any set
	if l.StartupMsg != "" && !l.TrainingMode && !l.Dry {
		if err := l.sendBotResponse(bot.Response{Send: true, Text: l.StartupMsg}, l.chatID, NotificationSilent); err != nil {
			log.Printf("[WARN] failed to send startup message, %v", err)
		} else {
			log.Printf("[DEBUG] startup message sent")
		}
	}

	// SpamFilter exposes explicit approval provenance. Keep the broader approved
	// check as a conservative fallback for any alternate Bot implementation.
	isExplicitlyApproved := l.Bot.IsApprovedUser
	if checker, ok := l.Bot.(interface{ IsExplicitTrustedUser(int64) bool }); ok {
		isExplicitlyApproved = checker.IsExplicitTrustedUser
	}
	l.adminHandler = &admin{
		tbAPI: l.TbAPI, bot: l.Bot, locator: l.Locator, superUsers: l.SuperUsers,
		primChatID: l.chatID, adminChatID: l.adminChatID,
		trainingMode: l.TrainingMode, softBan: l.SoftBanMode, dry: l.Dry, warnMsg: l.WarnMsg,
		aggressiveCleanup: l.AggressiveCleanup, aggressiveCleanupLimit: l.AggressiveCleanupLimit,
		isExplicitlyApproved: isExplicitlyApproved,
	}
	if l.RecoveryJournal != nil {
		if l.adminChatID == 0 {
			return fmt.Errorf("durable moderation requires an administrator notification destination")
		}
		api, ok := l.TbAPI.(contextualTelegram)
		if !ok {
			return fmt.Errorf("durable moderation requires context-aware Telegram API")
		}
		chatName := ""
		if chat, err := l.TbAPI.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: l.chatID}}); err == nil {
			chatName = chat.Title
		}
		adapter := &recovery{chatName: chatName, journal: l.RecoveryJournal, api: api,
			gate: func(a moderation.Action) bool {
				if a.Kind == "notice" || a.Kind == "card" {
					return a.ChatID == l.adminChatID && l.adminChatID != 0
				}
				if a.Kind == "unban" || a.Kind == "restore" {
					return !l.Dry && !l.TrainingMode && l.isChatAllowed(a.ChatID)
				}
				return !l.Dry && !l.TrainingMode && l.isChatAllowed(a.ChatID) &&
					!l.SuperUsers.IsSuper("", a.IdentityID) && !isExplicitlyApproved(a.IdentityID) &&
					(a.IdentityID != a.ChatID || a.Kind == "original_delete")
			}, superName: l.SuperUsers.IsSuper}
		l.adminHandler.recovery = adapter
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			w := moderation.Worker{Journal: l.RecoveryJournal, Executor: adapter}
			_ = w.Run(workerCtx)
		}()
		defer func() { stop(); <-done }()
	}

	l.reportsHandler = &userReports{
		ReportConfig: l.ReportConfig,
		tbAPI:        l.TbAPI, bot: l.Bot, locator: l.Locator, superUsers: l.SuperUsers,
		primChatID: l.chatID, adminChatID: l.adminChatID,
		trainingMode: l.TrainingMode, softBanMode: l.SoftBanMode, dry: l.Dry,
	}

	if l.RecoveryJournal != nil {
		l.reportsHandler.recoveryAdmin = l.adminHandler
	}

	adminForwardStatus := "enabled"
	if l.DisableAdminSpamForward {
		adminForwardStatus = "disabled"
	}
	log.Printf("[DEBUG] admin handler created, spam forwarding %s, %+v", adminForwardStatus, l.adminHandler)

	if l.AggressiveCleanup {
		log.Printf("[INFO] aggressive cleanup enabled, messages from user will be deleted on ban, limit %d",
			l.AggressiveCleanupLimit)
	}

	u := tbapi.NewUpdate(0)
	u.Timeout = 60

	updates := l.TbAPI.GetUpdatesChan(u)
	log.Printf("[DEBUG] start listening for updates")
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("listener context canceled: %w", ctx.Err())

		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("telegram update chan closed")
			}

			// handle admin chat messages. can be just messages (MsgHandler will ignore those)
			// or forwards of undetected spam by admins to admin's chat (in this case MsgHandler will process them and ban/train)
			if update.Message != nil && update.Message.Chat.ID == l.adminChatID {
				fromName, fromID := "", int64(0)
				if update.Message.From != nil {
					fromName, fromID = update.Message.From.UserName, update.Message.From.ID
				}
				if !l.isAdminChat(update.Message.Chat.ID, fromName, fromID) {
					continue
				}
				if l.DisableAdminSpamForward {
					log.Printf("[INFO] admin report ignored: outcome=forwarding_disabled admin_msg_id=%d admin_id=%d",
						update.Message.MessageID, fromID)
					continue
				}
				if err := l.adminHandler.MsgHandler(update); err != nil {
					log.Printf("[WARN] admin report failed: outcome=moderation_failed admin_msg_id=%d error=%v",
						update.Message.MessageID, err)
					errResp := l.sendBotResponse(
						bot.Response{Send: true, Text: "error: " + err.Error()},
						l.adminChatID,
						NotificationDefault,
					)
					if errResp != nil {
						log.Printf("[WARN] failed to respond on error, %v", errResp)
					}
				}
				continue
			}

			// handle admin chat inline buttons - route based on callback prefix
			if update.CallbackQuery != nil {
				callbackData := update.CallbackQuery.Data

				// delegate report callbacks (prefixes R+, R-, R?, R!, RX) to reportsHandler
				if len(callbackData) >= 3 && callbackData[:1] == "R" {
					if err := l.reportsHandler.HandleReportCallback(ctx, update.CallbackQuery); err != nil {
						log.Printf("[WARN] failed to process report callback: %v", err)
						errResp := l.sendBotResponse(
							bot.Response{Send: true, Text: "error: " + err.Error()},
							l.adminChatID,
							NotificationDefault,
						)
						if errResp != nil {
							log.Printf("[WARN] failed to respond on error, %v", errResp)
						}
					}
				} else {
					// all other callbacks (?, +, !, or no prefix) go to admin handler
					if err := l.adminHandler.InlineCallbackHandler(update.CallbackQuery); err != nil {
						log.Printf("[WARN] failed to process callback: %v", err)
						errResp := l.sendBotResponse(bot.Response{Send: true, Text: "error: " + err.Error()}, l.adminChatID, NotificationDefault)
						if errResp != nil {
							log.Printf("[WARN] failed to respond on error, %v", errResp)
						}
					}
				}
				continue
			}

			// handle edited messages
			if update.EditedMessage != nil {
				log.Printf("[INFO] processing edited message, id: %d", update.EditedMessage.MessageID)
				// we need to process an edited message as a new message, so we create a new update object
				// and copy the edited message to the message field.
				editedUpdate := tbapi.Update{
					Message: update.EditedMessage,
				}
				if err := l.procEvents(editedUpdate); err != nil {
					log.Printf("[WARN] failed to process edited message update: %v", err)
				}
				continue
			}

			if update.Message == nil {
				continue
			}

			if update.Message.NewChatMembers != nil {
				// handle join messages with mutually exclusive logic to prevent double-deletion:
				// - if DeleteJoinMessages=true: delete immediately, don't store in locator
				// - if DeleteJoinMessages=false: store in locator for potential later deletion via SuppressJoinMessage
				// this prevents "message not found" errors when both flags are enabled
				if l.DeleteJoinMessages {
					l.deleteSystemMessage(update.Message.MessageID, update.Message.Chat.ID, "join")
				} else {
					err := l.procNewChatMemberMessage(update)
					if err != nil {
						log.Printf("[WARN] failed to process new chat member: %v", err)
					}
				}
				continue
			}

			// handle left member messages, i.e. "blah blah removed from the chat"
			if update.Message.LeftChatMember != nil {
				if l.SuppressJoinMessage {
					// delete the stored join message when user leaves
					err := l.procLeftChatMemberMessage(update)
					if err != nil {
						log.Printf("[WARN] failed to process left chat member: %v", err)
					}
				}
				// immediately delete leave message if requested
				if l.DeleteLeaveMessages {
					l.deleteSystemMessage(update.Message.MessageID, update.Message.Chat.ID, "leave")
				}
				continue
			}

			if l.procSenderSpam(update) {
				continue
			}

			// handle spam reports from superusers
			fromSuper := l.SuperUsers.IsSuper(update.Message.From.UserName, update.Message.From.ID)
			if update.Message.ReplyToMessage != nil && fromSuper {
				if l.procSuperReply(update) {
					// superuser command processed, skip the rest
					continue
				}
			}

			// delete orphaned /report commands (sent without replying to a message)
			if !fromSuper && l.isReportCommand(update.Message.Text) && update.Message.ReplyToMessage == nil {
				log.Printf(
					"[DEBUG] deleting orphaned /report command from %s (%d)",
					update.Message.From.UserName,
					update.Message.From.ID,
				)
				_, err := l.TbAPI.Request(tbapi.DeleteMessageConfig{BaseChatMessage: tbapi.BaseChatMessage{
					MessageID:  update.Message.MessageID,
					ChatConfig: tbapi.ChatConfig{ChatID: update.Message.Chat.ID},
				}})
				if err != nil {
					log.Printf("[WARN] failed to delete orphaned /report message %d: %v", update.Message.MessageID, err)
				}
				continue
			}

			// handle spam reports from regular users
			if update.Message.ReplyToMessage != nil && !fromSuper {
				if l.procUserReply(ctx, update) {
					// user command processed, skip the rest
					continue
				}
			}

			// process regular messages, the main part of the bot
			if err := l.procEvents(update); err != nil {
				log.Printf("[WARN] failed to process update: %v", err)
				continue
			}

		case <-time.After(l.IdleDuration): // hit bots on idle timeout
			resp := l.Bot.OnMessage(bot.Message{Text: "idle"}, false)
			if err := l.sendBotResponse(resp, l.chatID, NotificationSilent); err != nil {
				log.Printf("[WARN] failed to respond on idle, %v", err)
			}
		}
	}
}

func (l *TelegramListener) procEvents(update tbapi.Update) error {
	fromChat := update.Message.Chat.ID
	// ignore messages from other chats except the one we are monitor and ones from the test list
	if !l.isChatAllowed(fromChat) {
		return nil
	}

	msg := transform(update.Message)
	hanText := bot.HanOwnText(update.Message.Text, hanEntities(update.Message.Entities))
	if update.Message.Caption != "" {
		if update.Message.Text != "" {
			hanText += "\n"
		}
		hanText += bot.HanOwnText(update.Message.Caption, hanEntities(update.Message.CaptionEntities))
	}
	if hanText != msg.Text {
		msg.HanText = &hanText
	}

	textPresent := strings.TrimSpace(msg.Text) != ""
	mediaKey, mediaStatus := extractMediaLocatorKey(update.Message)
	legacyContentAdmitted := msg.Image != nil || msg.WithVideoNote || msg.WithVideo || msg.WithForward ||
		msg.WithExternalLinkButton

	// external-link buttons must reach the deterministic guard even when Telegram exposes no text/media flag.
	if !textPresent && !legacyContentAdmitted && mediaStatus != mediaLocatorExtracted {
		return nil
	}
	ctx := context.TODO()
	log.Printf(
		"[DEBUG] incoming message: msg_id=%d chat_id=%d sender_id=%d "+
			"text_present=%t text_length=%d media_status=%s forward=%t external_button=%t",
		msg.ID,
		fromChat,
		msg.From.ID,
		textPresent,
		len([]rune(msg.Text)),
		mediaStatus,
		msg.WithForward,
		msg.WithExternalLinkButton,
	)

	// use channel identity for locator when message is sent on behalf of a channel
	locatorUserID := msg.From.ID
	locatorUserName := msg.From.Username
	if msg.SenderChat.ID != 0 {
		locatorUserID = msg.SenderChat.ID
		locatorUserName = msg.SenderChat.UserName
	}
	if textPresent {
		if err := l.Locator.AddMessage(ctx, msg.Text, fromChat, locatorUserID, locatorUserName, msg.ID); err != nil {
			log.Printf("[WARN] failed to add message to locator: %v", err)
		}
	} else if mediaStatus == mediaLocatorExtracted {
		identityKind := storage.LocatorIdentityUser
		if locatorUserID < 0 {
			identityKind = storage.LocatorIdentitySenderChat
		}
		identity := storage.LocatorIdentity{Kind: identityKind, ID: locatorUserID}
		if err := l.Locator.AddMediaMessage(ctx, mediaKey, fromChat, identity, locatorUserName, msg.ID); err != nil {
			log.Printf("[WARN] failed to add media message to locator: kind=%s msg_id=%d: %v", mediaKey.Kind, msg.ID, err)
		} else {
			log.Printf(
				"[DEBUG] media locator registered: kind=%s version=%d msg_id=%d target_id=%d",
				mediaKey.Kind, mediaKey.Version, msg.ID, locatorUserID,
			)
		}
	}

	if dated, ok := l.Locator.(messageDateLocator); ok {
		if err := dated.RecordMessageDate(ctx, fromChat, msg.ID, int64(update.Message.Date)); err != nil {
			log.Printf("[WARN] failed to record message date: chat_id=%d message_id=%d: %v", fromChat, msg.ID, err)
		}
	}

	// document and animation are admitted here only to make their captionless source locatable.
	// preserve the old direct-moderation path by not sending newly admitted empty content to Bot.OnMessage.
	if !textPresent && !legacyContentAdmitted {
		return nil
	}

	// skip spam check for anonymous admin posts from this group
	// when admins post "as the group", SenderChat.ID equals the group's chat ID
	if msg.SenderChat.ID != 0 && msg.SenderChat.ID == fromChat {
		log.Printf("[DEBUG] skipping spam check for anonymous admin post from group itself")
		return nil
	}

	// telegram group administrators are loaded into SuperUsers by updateSupers. They are exempt from
	// this guard; approved/trusted non-admin users are deliberately not exempt.
	if msg.WithExternalLinkButton && l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) {
		log.Printf("[DEBUG] external inline button guard bypassed for administrator %s (%d)", msg.From.Username, msg.From.ID)
		msg.WithExternalLinkButton = false
	}

	resp := l.Bot.OnMessage(*msg, false)

	if resp.HanMatched || resp.HanDeleteOnly {
		if l.Dry || l.TrainingMode {
			return nil
		}
		if l.RecoveryJournal == nil || l.adminHandler == nil || l.adminHandler.recovery == nil {
			return fmt.Errorf("han deletion requires durable moderation journal")
		}
		hanID := int64(0)
		if resp.HanMatched {
			hanID, _, _ = hanOriginalIdentity(update.Message)
		}
		if resp.HanOnly || resp.HanDeleteOnly || resp.HanMatched && hanID == 0 {
			_, err := l.enqueueAutomatic(update, msg, resp)
			return err
		}
	}

	if !resp.Send { // not spam
		return nil
	}
	if l.RecoveryJournal != nil && !l.Dry && !l.TrainingMode && needsRecovery(resp) {
		created, err := l.enqueueAutomatic(update, msg, resp)
		if created && resp.BanInterval > 0 {
			l.SpamLogger.Save(msg, &resp)
			id := msg.From.ID
			if msg.SenderChat.ID != 0 {
				id = msg.SenderChat.ID
			}
			if e := l.Locator.AddSpam(ctx, id, resp.CheckResults); e != nil {
				log.Printf("[WARN] durable moderation evidence not saved")
			}
		}
		if created && !l.NoSpamReply {
			// acceptance precedes this optional reply; its failure cannot lose durable moderation.
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, e := l.adminHandler.recovery.api.RequestWithContext(
				callCtx,
				tbapi.NewMessage(
					fromChat,
					"Moderation recorded; API outcomes are pending. See the administrator journal summary.",
				),
			)
			cancel()
			if e != nil {
				log.Printf("[WARN] optional moderation reply unavailable; durable admin notice retained")
			}
		}
		return err
	}

	// send response to the channel if allowed
	if resp.Send && !l.NoSpamReply && !l.TrainingMode {
		if err := l.sendBotResponse(resp, fromChat, NotificationSilent); err != nil {
			log.Printf("[WARN] failed to respond on update, %v", err)
		}
	}

	errs := new(multierror.Error)
	ban := actionResult{Status: "not_attempted", Reason: "not requested"}
	original := actionResult{Status: "not_attempted", Reason: "not requested or safety gate"}
	banUserStr := l.getBanUsername(resp, update)

	// ban user if requested by bot
	if resp.Send && resp.BanInterval > 0 {
		log.Printf("[DEBUG] ban initiated for %+v", resp)
		l.SpamLogger.Save(msg, &resp)
		spamUserID := msg.From.ID
		if msg.SenderChat.ID != 0 {
			spamUserID = msg.SenderChat.ID
		}
		if err := l.Locator.AddSpam(ctx, spamUserID, resp.CheckResults); err != nil {
			log.Printf("[WARN] failed to add spam to locator: %v", err)
		}

		if l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) {
			if l.TrainingMode && l.adminHandler != nil && l.adminChatID != 0 {
				if err := l.adminHandler.reportAutomatic(banUserStr, msg,
					actionResult{Status: "not_attempted", Reason: "superuser safety gate"}, original,
					cleanupResult{}, !l.Dry); err != nil {
					log.Printf("[WARN] training notification: %v", err)
				}
			}
			log.Printf("[DEBUG] superuser %s requested ban, ignored", banUserStr)
			return nil
		}

		banReq := banRequest{
			duration:  resp.BanInterval,
			userID:    resp.User.ID,
			channelID: resp.ChannelID,
			userName:  banUserStr,
			chatID:    fromChat,
			dry:       l.Dry,
			training:  l.TrainingMode,
			tbAPI:     l.TbAPI,
			restrict:  l.SoftBanMode,
		}
		var banErr error
		ban, banErr = banResult(banReq)
		if banErr != nil {
			errs = multierror.Append(errs, banErr)
		}

	}

	// delete extra messages if spam detected (e.g., duplicates)
	cleanup := l.deleteExtraMessages(resp.CheckResults, msg.From.ID, msg.From.Username, fromChat)

	// delete message if requested by bot
	canDelete := resp.DeleteReplyTo && resp.ReplyTo != 0 && !l.Dry &&
		!l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) && !l.TrainingMode
	if canDelete {
		sentAt := int64(update.Message.Date)
		if resp.ReplyTo != msg.ID {
			sentAt = locatorMessageDate(l.Locator, fromChat, resp.ReplyTo)
		}
		var deleteErr error
		original, deleteErr = deleteOriginal(l.TbAPI, l.chatID, resp.ReplyTo, sentAt, time.Now())
		if deleteErr != nil {
			errs = multierror.Append(errs, deleteErr)
		}
	}
	log.Printf("[INFO] automatic moderation result: user_id=%d chat_id=%d message_id=%d ban=%s delete=%s cleanup=%s",
		msg.From.ID, fromChat, msg.ID, ban, original, cleanup)
	if l.adminHandler != nil && l.adminChatID != 0 && (resp.BanInterval > 0 || canDelete || cleanup.Selected > 0) {
		allowControls := !l.Dry && msg.From.ID != 0 &&
			(ban.Status == "confirmed" || l.TrainingMode && resp.BanInterval > 0)
		if err := l.adminHandler.reportAutomatic(banUserStr, msg, ban, original, cleanup, allowControls); err != nil {
			errs = multierror.Append(errs, err)
		}
	}

	if err := errs.ErrorOrNil(); err != nil {
		return fmt.Errorf("processing events failed: %w", err)
	}
	return nil
}

// procSuperReply processes superuser commands (reply) /spam, /ban, /warn
func (l *TelegramListener) procSuperReply(update tbapi.Update) (handled bool) {
	switch {
	case strings.EqualFold(update.Message.Text, "/spam") || strings.EqualFold(update.Message.Text, "spam"):
		log.Printf("[DEBUG] superuser %s reported spam", update.Message.From.UserName)
		if err := l.adminHandler.DirectSpamReport(update); err != nil {
			log.Printf("[WARN] failed to process direct spam report: %v", err)
		}
		return true
	case strings.EqualFold(update.Message.Text, "/ban") || strings.EqualFold(update.Message.Text, "ban"):
		log.Printf("[DEBUG] superuser %s requested ban", update.Message.From.UserName)
		if err := l.adminHandler.DirectBanReport(update); err != nil {
			log.Printf("[WARN] failed to process direct ban request: %v", err)
		}
		return true
	case strings.EqualFold(update.Message.Text, "/warn") || strings.EqualFold(update.Message.Text, "warn"):
		log.Printf("[DEBUG] superuser %s requested warning", update.Message.From.UserName)
		if err := l.adminHandler.DirectWarnReport(update); err != nil {
			log.Printf("[WARN] failed to process direct warning request: %v", err)
		}
		return true
	}
	return false
}

// isReportCommand checks if message text is a /report command variant
func (l *TelegramListener) isReportCommand(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))

	// exact match for "report" or "/report"
	if text == "report" || text == "/report" {
		return true
	}

	// handle "/report@botname" syntax
	if strings.HasPrefix(text, "/report@") {
		// extract everything after "@"
		afterAt := text[8:] // skip "/report@"

		// reject empty username or whitespace-only
		fields := strings.Fields(afterAt)
		if len(fields) == 0 {
			return false
		}

		// extract just the username (up to space or end of string)
		// handles cases like "/report@bot" and "/report@bot some text"
		username := fields[0]

		// if bot username not configured, reject @ commands for security
		if l.BotUsername == "" {
			return false
		}

		// case-insensitive comparison (telegram usernames are case-insensitive)
		return strings.EqualFold(username, l.BotUsername)
	}

	return false
}

// procUserReply processes regular user commands (reply) /report.
// feature check is intentionally inside this function to keep command detection logic centralized.
func (l *TelegramListener) procUserReply(ctx context.Context, update tbapi.Update) (handled bool) {
	switch {
	case l.isReportCommand(update.Message.Text):
		if !l.ReportConfig.Enabled {
			log.Printf("[DEBUG] user spam reporting disabled, ignoring /report from %s (%d)",
				update.Message.From.UserName, update.Message.From.ID)
			return true // command is suppressed when feature is disabled
		}
		log.Printf("[DEBUG] user %s (%d) reported spam", update.Message.From.UserName, update.Message.From.ID)
		if err := l.reportsHandler.DirectUserReport(ctx, update); err != nil {
			log.Printf("[WARN] failed to process user spam report: %v", err)
		}
		return true
	}
	return false
}

// procNewChatMemberMessage saves new chat member message to locator. It is used to delete the message if the user kicked out
func (l *TelegramListener) procNewChatMemberMessage(update tbapi.Update) error {
	fromChat := update.Message.Chat.ID
	// ignore messages from other chats except the one we are monitor and ones from the test list
	if !l.isChatAllowed(fromChat) {
		return nil
	}

	if len(update.Message.NewChatMembers) != 1 {
		log.Printf("[DEBUG] we are expecting only one new chat member, got %d", len(update.Message.NewChatMembers))
		return nil
	}

	errs := new(multierror.Error)

	member := update.Message.NewChatMembers[0]
	msg := fmt.Sprintf("new_%d_%d", fromChat, member.ID)
	if err := l.Locator.AddMessage(context.TODO(), msg, fromChat, member.ID, "", update.Message.MessageID); err != nil {
		errs = multierror.Append(errs, fmt.Errorf("failed to add new chat member message to locator: %w", err))
	}

	if err := errs.ErrorOrNil(); err != nil {
		return fmt.Errorf("failed to process new chat member: %w", err)
	}
	return nil
}

// procLeftChatMemberMessage deletes the message about new chat member if the user kicked out
func (l *TelegramListener) procLeftChatMemberMessage(update tbapi.Update) error {
	fromChat := update.Message.Chat.ID
	// ignore messages from other chats except the one we are monitor and ones from the test list
	if !l.isChatAllowed(fromChat) {
		return nil
	}

	if update.Message.From.ID == update.Message.LeftChatMember.ID {
		log.Printf("[DEBUG] left chat member is the same as the message sender, ignored")
		return nil
	}
	msg, found := l.Locator.Message(context.TODO(), fmt.Sprintf("new_%d_%d", fromChat, update.Message.LeftChatMember.ID))
	if !found {
		log.Printf("[DEBUG] no new chat member message found for %d in chat %d", update.Message.LeftChatMember.ID, fromChat)
		return nil
	}
	if _, err := l.TbAPI.Request(tbapi.DeleteMessageConfig{
		BaseChatMessage: tbapi.BaseChatMessage{ChatConfig: tbapi.ChatConfig{ChatID: fromChat}, MessageID: msg.MsgID},
	}); err != nil {
		return fmt.Errorf("failed to delete new chat member message %d: %w", msg.MsgID, err)
	}

	return nil
}

// deleteSystemMessage deletes a system message immediately
func (l *TelegramListener) deleteSystemMessage(msgID int, chatID int64, msgType string) {
	deleteMsg := tbapi.DeleteMessageConfig{
		BaseChatMessage: tbapi.BaseChatMessage{
			MessageID:  msgID,
			ChatConfig: tbapi.ChatConfig{ChatID: chatID},
		},
	}
	if _, err := l.TbAPI.Request(deleteMsg); err != nil {
		log.Printf("[WARN] failed to delete %s message %d: %v", msgType, msgID, err)
	} else {
		log.Printf("[DEBUG] %s message %d deleted", msgType, msgID)
	}
}

func (l *TelegramListener) isChatAllowed(fromChat int64) bool {
	if fromChat == l.chatID {
		return true
	}
	return slices.Contains(l.TestingIDs, fromChat)
}

func (l *TelegramListener) isAdminChat(fromChat int64, from string, fromID int64) bool {
	if fromChat == l.adminChatID {
		log.Printf("[DEBUG] admin report routed: outcome=admin_chat_received chat_id=%d admin_id=%d", fromChat, fromID)
		if !l.SuperUsers.IsSuper(from, fromID) {
			log.Printf("[WARN] admin report rejected: outcome=unauthorized_sender chat_id=%d admin_id=%d", fromChat, fromID)
			return false
		}
		return true
	}
	return false
}

func (l *TelegramListener) getBanUsername(resp bot.Response, update tbapi.Update) string {
	if resp.ChannelID == 0 {
		return formatBanUser(resp.User)
	}
	botChat := bot.SenderChat{
		ID: resp.ChannelID,
	}
	if update.Message.SenderChat != nil {
		botChat.UserName = update.Message.SenderChat.UserName
	}
	// if botChat.UserName not set, that means the ban comes from superuser and username should be taken from ReplyToMessage
	if botChat.UserName == "" && update.Message.ReplyToMessage != nil && update.Message.ReplyToMessage.SenderChat != nil {
		if update.Message.ReplyToMessage.ForwardOrigin != nil {
			if update.Message.ReplyToMessage.ForwardOrigin.IsUser() {
				botChat.UserName = update.Message.ReplyToMessage.ForwardOrigin.SenderUser.UserName
			}
			if update.Message.ReplyToMessage.ForwardOrigin.IsHiddenUser() {
				botChat.UserName = update.Message.ReplyToMessage.ForwardOrigin.SenderUserName
			}
		}
	}
	return fmt.Sprintf("%v", botChat)
}
func formatBanUser(user bot.User) string {
	parts := []string{fmt.Sprintf("%d", user.ID)}
	if user.Username != "" {
		parts = append(parts, user.Username)
	}

	displayName := strings.TrimSpace(user.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(strings.TrimSpace(user.FirstName) + " " + strings.TrimSpace(user.LastName))
	}
	if displayName != "" {
		parts = append(parts, displayName)
	}

	return strings.Join(parts, " ")
}

// NotificationType defines how a message is delivered to users
type NotificationType int

const (
	// NotificationDefault sends message with standard notification
	NotificationDefault NotificationType = iota
	// NotificationSilent sends message without sound
	NotificationSilent
)

// sendBotResponse sends bot's answer to tg channel
// actionText is a text for the button to unban user, optional
func (l *TelegramListener) sendBotResponse(resp bot.Response, chatID int64, notifyType NotificationType) error {
	if !resp.Send {
		return nil
	}

	log.Printf("[DEBUG] bot response - %+v, reply-to:%d", strings.ReplaceAll(resp.Text, "\n", "\\n"), resp.ReplyTo)
	tbMsg := tbapi.NewMessage(chatID, resp.Text)
	tbMsg.ParseMode = tbapi.ModeMarkdown
	tbMsg.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
	tbMsg.ReplyParameters = tbapi.ReplyParameters{MessageID: resp.ReplyTo}
	tbMsg.DisableNotification = notifyType == NotificationSilent

	if err := send(tbMsg, l.TbAPI); err != nil {
		return fmt.Errorf("can't send message to telegram %q: %w", resp.Text, err)
	}

	return nil
}

func (l *TelegramListener) getChatID(group string) (int64, error) {
	chatID, err := strconv.ParseInt(group, 10, 64)
	if err == nil {
		return chatID, nil
	}

	chat, err := l.TbAPI.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{SuperGroupUsername: "@" + group}})
	if err != nil {
		return 0, fmt.Errorf("can't get chat for %s: %w", group, err)
	}

	return chat.ID, nil
}

// updateSupers updates the list of super-users based on the chat administrators fetched from the Telegram API.
// it uses the user ID first, but can match by username if set in the list of super-users.
func (l *TelegramListener) updateSupers() error {
	isSuper := func(username string, id int64) bool {
		for _, super := range l.SuperUsers {
			if super == fmt.Sprintf("%d", id) {
				return true
			}
			if username != "" && super == username {
				return true
			}
		}
		return false
	}

	admins, err := l.TbAPI.GetChatAdministrators(tbapi.ChatAdministratorsConfig{ChatConfig: tbapi.ChatConfig{ChatID: l.chatID}})
	if err != nil {
		return fmt.Errorf("failed to get chat administrators: %w", err)
	}

	for _, admin := range admins {
		if admin.User.UserName == "" && admin.User.ID == 0 {
			continue
		}
		if isSuper(admin.User.UserName, admin.User.ID) {
			continue // already in the list
		}
		l.SuperUsers = append(l.SuperUsers, fmt.Sprintf("%d", admin.User.ID))
	}

	log.Printf("[INFO] added admins, full list of supers: {%s}", strings.Join(l.SuperUsers, ", "))
	return nil
}

// deleteExtraMessages deletes additional messages specified in check results (e.g., duplicate messages)
func (l *TelegramListener) deleteExtraMessages(checkResults []spamcheck.Response, userID int64,
	username string, chatID int64) cleanupResult {
	result := cleanupResult{}
	if len(checkResults) == 0 || l.Dry || l.TrainingMode || l.SuperUsers.IsSuper(username, userID) {
		return result
	}
	for _, check := range checkResults {
		if !check.Spam {
			continue
		}
		for _, id := range check.ExtraDeleteIDs {
			result.Selected++
			time.Sleep(35 * time.Millisecond)
			item, err := deleteOriginal(l.TbAPI, chatID, id, locatorMessageDate(l.Locator, chatID, id), time.Now())
			result.add(item)
			if err != nil {
				log.Printf("[WARN] extra_delete_failed: %v", err)
			}
		}
	}
	return result
}

// SuperUsers for moderators. Can be either username or user ID.
type SuperUsers []string

// IsSuper checks if userID or username in the list of superusers
// First it treats super as user ID, then as username
func (s SuperUsers) IsSuper(userName string, userID int64) bool {
	for _, super := range s {
		if id, err := strconv.ParseInt(super, 10, 64); err == nil {
			// super is user ID
			if userID == id {
				return true
			}
			continue
		}
		// super is username
		if strings.EqualFold(userName, super) || strings.EqualFold("/"+userName, super) {
			return true
		}
	}
	return false
}
