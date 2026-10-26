package events

import (
	"context"
	"fmt"
	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/hashicorp/go-multierror"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"log"
	"strings"
	"time"
)

// Historical oracle for tests of pre-existing delete-only/request history and old
// identity/learning fixtures. It is compiled only in tests; production has one
// forward handler (admin_forward.go).
//
// The old handler handled messages received on admin chat. this is usually forwarded spam failed
// to be detected by the bot. we need to update spam filter with this message and ban the user.
// the user will be banned even in training mode, but not in the dry mode.
// if the locator lookup fails but ForwardOrigin provides the original sender's user ID,
// a degraded fallback path is used: the user is banned and spam samples updated,
// but the original message cannot be deleted automatically.
func (a *admin) legacyForward(update tbapi.Update) error {
	if update.Message == nil {
		log.Printf("[WARN] admin report blocked: outcome=missing_message update_id=%d", update.UpdateID)
		return nil
	}

	adminID := int64(0)
	if update.Message.From != nil {
		adminID = update.Message.From.ID
	}
	log.Printf("[INFO] admin report received: admin_msg_id=%d update_id=%d admin_id=%d",
		update.Message.MessageID, update.UpdateID, adminID)

	if update.Message.ForwardOrigin == nil {
		// regular messages in the admin chat are intentionally ignored. Keep this quiet for
		// the user, but leave a stable marker so delivery can be distinguished from absence.
		log.Printf("[DEBUG] admin report ignored: outcome=missing_origin admin_msg_id=%d", update.Message.MessageID)
		return nil
	}

	if locator, ok := a.locator.(deleteCandidateLocator); ok {
		return a.originalDeleteCandidates(update, locator)
	}
	origin := adminForwardOriginIdentity(update.Message.ForwardOrigin)
	log.Printf("[DEBUG] admin report origin parsed: admin_msg_id=%d origin_type=%s target_type=%s target_id=%d reliable=%t",
		update.Message.MessageID, origin.OriginType, origin.Kind, origin.ID, origin.Reliable)
	if !origin.Reliable {
		return a.blockAdminReport(update, "origin_unavailable",
			"Не удалось определить отправителя пересылки.\n"+
				"Ответьте командой /spam на оригинал в исходном чате.\n"+
				"Команда доступна только уполномоченному администратору.")
	}

	// this is a forwarded message from super to admin chat, it is an example of missed spam
	// we need to update spam filter with this message
	msgTxt := update.Message.Text
	if msgTxt == "" { // if no text, try to get it from the transformed message
		m := transform(update.Message)
		msgTxt = m.Text
	}

	mediaOnly := msgTxt == ""
	var info storage.MsgMeta
	var ok bool
	if mediaOnly {
		mediaKey, mediaStatus := extractMediaLocatorKey(update.Message)
		if mediaStatus != mediaLocatorExtracted {
			outcome := "media_key_unavailable"
			if mediaStatus == mediaLocatorUnsupported || mediaStatus == mediaLocatorAlbum {
				outcome = "media_type_unsupported"
			}
			log.Printf("[WARN] admin report blocked: outcome=%s admin_msg_id=%d origin_type=%s target_id=%d media_status=%s",
				outcome, update.Message.MessageID, origin.OriginType, origin.ID, mediaStatus)
			return a.blockAdminReport(update, outcome,
				"Unable to identify the forwarded media safely. No automatic moderation action was taken.")
		}
		identity, identityOK := storageIdentity(origin)
		if !identityOK {
			return a.blockAdminReport(update, "media_identity_unavailable",
				"Unable to determine the forwarded media sender reliably. No automatic moderation action was taken.")
		}
		log.Printf(
			"[DEBUG] admin report media key extracted: outcome=media_key_extracted "+
				"admin_msg_id=%d media_kind=%s key_version=%d target_type=%s target_id=%d",
			update.Message.MessageID,
			mediaKey.Kind,
			mediaKey.Version,
			origin.Kind,
			origin.ID,
		)
		info, ok = a.locator.MessageByMedia(context.TODO(), mediaKey, a.primChatID, identity)
		if !ok {
			log.Printf("[INFO] admin report media locator result: outcome=media_locator_miss "+
				"admin_msg_id=%d media_kind=%s target_type=%s target_id=%d",
				update.Message.MessageID, mediaKey.Kind, origin.Kind, origin.ID)
			return a.blockAdminReport(update, "media_locator_miss",
				"The original media message could not be located safely. No automatic moderation action was taken.")
		}
		log.Printf("[INFO] admin report media locator result: outcome=media_locator_hit "+
			"admin_msg_id=%d original_msg_id=%d media_kind=%s target_type=%s target_id=%d",
			update.Message.MessageID, info.MsgID, mediaKey.Kind, origin.Kind, origin.ID)
	} else {
		// textual and captioned reports keep the established Phase 1 path.
		info, ok = a.locator.Message(context.TODO(), msgTxt)
	}
	if !ok {
		log.Printf("[INFO] admin report locator result: outcome=locator_miss admin_msg_id=%d origin_type=%s target_id=%d",
			update.Message.MessageID, origin.OriginType, origin.ID)
		// preserve the existing degraded fallback only for a reliable Telegram user.
		if origin.Kind == adminReportIdentityUser {
			if a.targetExplicitlyApproved(origin.ID) {
				return a.blockAdminReport(
					update,
					"approved_target",
					"Report target is explicitly approved. Automatic spam action was blocked. Remove approval explicitly before retrying.",
				)
			}
			log.Printf("[WARN] admin report degraded fallback: admin_msg_id=%d target_type=%s target_id=%d",
				update.Message.MessageID, origin.Kind, origin.ID)
			return a.msgHandlerFallback(update, origin.ID, origin.Name, msgTxt)
		}
		return a.blockAdminReport(
			update,
			"locator_miss_unsupported_origin",
			"The original message could not be located safely for this sender type. No automatic moderation action was taken.",
		)
	}

	locatorIdentity := adminLocatorIdentity(info)
	if mediaOnly && info.ChatID != a.primChatID {
		log.Printf("[WARN] admin report media source mismatch; destructive action "+
			"blocked: outcome=media_identity_mismatch admin_msg_id=%d original_msg_id=%d source_chat_id=%d",
			update.Message.MessageID, info.MsgID, info.ChatID)
		return a.blockAdminReport(update, "media_identity_mismatch",
			"Forwarded media source does not match the protected chat. Automatic moderation was blocked.")
	}
	log.Printf("[INFO] admin report locator result: outcome=locator_hit "+
		"admin_msg_id=%d original_msg_id=%d target_type=%s target_id=%d",
		update.Message.MessageID, info.MsgID, locatorIdentity.Kind, locatorIdentity.ID)
	if !adminReportIdentitiesMatch(origin, locatorIdentity) {
		outcome := "identity_mismatch"
		if mediaOnly {
			outcome = "media_identity_mismatch"
		}
		log.Printf("[WARN] admin report locator identity mismatch; destructive action "+
			"blocked: admin_msg_id=%d original_msg_id=%d origin_type=%s origin_id=%d locator_type=%s locator_id=%d",
			update.Message.MessageID, info.MsgID, origin.Kind, origin.ID, locatorIdentity.Kind, locatorIdentity.ID)
		return a.blockAdminReport(update, outcome,
			"Forwarded sender identity does not match the located source. Automatic moderation was blocked.")
	}

	errs := new(multierror.Error)

	// check if the forwarded message will ban a super-user and ignore it
	if a.superUsers.IsSuper(info.UserName, info.UserID) {
		return fmt.Errorf("forwarded message is about super-user %s (%d), ignored", info.UserName, info.UserID)
	}
	if a.targetExplicitlyApproved(info.UserID) {
		outcome := "approved_target"
		if mediaOnly {
			outcome = "media_approved_block"
		}
		return a.blockAdminReport(
			update,
			outcome,
			"Report target is explicitly approved. Automatic spam action was blocked. Remove approval explicitly before retrying.",
		)
	}

	if a.recovery != nil && !a.dry && !a.trainingMode {
		if info.ChatID != a.primChatID {
			return a.blockAdminReport(
				update,
				"durable_source_mismatch",
				"Located source chat is not the primary chat; no durable action accepted.",
			)
		}
		exact := update.Message.ForwardOrigin.Type == "channel" &&
			update.Message.ForwardOrigin.Chat != nil && update.Message.ForwardOrigin.Chat.ID == info.ChatID &&
			update.Message.ForwardOrigin.MessageID == info.MsgID
		learning := msgTxt
		if mediaOnly {
			learning = ""
		}
		return a.enqueueForward(update, info.UserID, info.MsgID, exact, learning, a.aggressiveCleanup)
	}

	// remove user from the approved list and from storage
	if err := a.bot.RemoveApprovedUser(info.UserID); err != nil {
		log.Printf("[WARN] failed to remove user %d from approved list: %v", info.UserID, err)
	}

	// make a message with spam info and send to admin chat
	spamInfo := []string{}
	// check only, don't update the storage, as all we care here is to get checks results.
	// without checkOnly flag, it may add approved user to the storage after we removed it above.
	spamInfoText := "**can't get spam info**"
	if mediaOnly {
		spamInfoText = "**captionless media: textual detection and learning skipped**"
	} else {
		resp := a.bot.OnMessage(bot.Message{Text: update.Message.Text, From: bot.User{ID: info.UserID}}, true)
		for _, check := range resp.CheckResults {
			spamInfo = append(spamInfo, "- "+escapeMarkDownV1Text(check.String()))
		}
		if len(spamInfo) > 0 {
			spamInfoText = strings.Join(spamInfo, "\n")
		}
	}
	heading := fmt.Sprintf("**original detection results for %q (%d)**\n\n%s",
		escapeMarkDownV1Text(info.UserName), info.UserID, spamInfoText)
	ban := actionResult{Status: "not_attempted", Reason: "dry mode"}
	original := actionResult{Status: "not_attempted", Reason: "dry mode"}
	cleanup := "not scheduled"
	if a.dry {
		return a.finishModeration(heading+moderationText(ban, original, cleanup), nil)
	}
	if !mediaOnly {
		if err := a.bot.UpdateSpam(msgTxt); err != nil {
			return a.finishModeration(heading+moderationText(
				actionResult{Status: "not_attempted", Reason: "learning failed"},
				actionResult{Status: "not_attempted", Reason: "learning failed"}, cleanup),
				fmt.Errorf("learning_failed: %w", err))
		}
	}
	original, err := deleteOriginal(a.tbAPI, a.primChatID, info.MsgID,
		locatorMessageDate(a.locator, a.primChatID, info.MsgID), time.Now())
	if err != nil {
		errs = multierror.Append(errs, err)
	}
	ban = actionResult{Status: "not_attempted", Reason: "anonymous admin post"}
	if info.UserID != a.primChatID {
		banReq := banRequest{duration: bot.PermanentBanDuration, userID: info.UserID,
			channelID: channelIDFromCallback(info.UserID), chatID: a.primChatID,
			tbAPI: a.tbAPI, dry: a.dry, training: a.trainingMode, userName: origin.Name}
		ban, err = banResult(banReq)
		if err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	if a.aggressiveCleanup && info.UserID != a.primChatID {
		cleanup = "pending; final result follows"
	}
	log.Printf("[INFO] admin moderation result: chat_id=%d message_id=%d ban=%s delete=%s cleanup=%s",
		a.primChatID, original.MessageID, ban, original, cleanup)
	reportErr := a.finishModeration(heading+moderationText(ban, original, cleanup), errs.ErrorOrNil())
	if a.aggressiveCleanup && info.UserID != a.primChatID {
		a.startCleanup(info.UserID, info.UserName)
	}
	if reportErr != nil {
		return reportErr
	}

	log.Printf(
		"[INFO] admin report handled: outcome=moderation_handled admin_msg_id=%d original_msg_id=%d target_type=%s target_id=%d",
		update.Message.MessageID,
		info.MsgID,
		locatorIdentity.Kind,
		locatorIdentity.ID,
	)
	if mediaOnly {
		log.Printf("[INFO] admin report media handled: "+
			"outcome=media_handled_without_learning admin_msg_id=%d original_msg_id=%d target_type=%s target_id=%d",
			update.Message.MessageID, info.MsgID, locatorIdentity.Kind, locatorIdentity.ID)
	}
	return nil
}

// msgHandlerFallback handles the degraded fallback path when the locator lookup fails
// but ForwardOrigin provides the original sender's user ID. performs all possible actions
// (ban, spam update, remove from approved) and warns the admin that the original message
// must be deleted manually since we don't have the message ID from the primary chat.
func (a *admin) msgHandlerFallback(update tbapi.Update, fwdID int64, username, msgTxt string) error {
	log.Printf("[INFO] admin report fallback started: admin_msg_id=%d target_type=%s target_id=%d",
		update.Message.MessageID, adminReportIdentityUser, fwdID)
	errs := new(multierror.Error)

	// check if the forwarded user is a super-user and ignore if so
	if a.superUsers.IsSuper(username, fwdID) {
		return fmt.Errorf("forwarded message is about super-user %s (%d), ignored", username, fwdID)
	}
	if a.recovery != nil && !a.dry && !a.trainingMode {
		return a.enqueueForward(update, fwdID, 0, false, msgTxt, false)
	}

	// remove user from the approved list
	if err := a.bot.RemoveApprovedUser(fwdID); err != nil {
		log.Printf("[WARN] failed to remove user %d from approved list: %v", fwdID, err)
	}

	// get detection results (check only, don't update storage)
	spamInfo := []string{}
	resp := a.bot.OnMessage(bot.Message{Text: update.Message.Text, From: bot.User{ID: fwdID}}, true)
	spamInfoText := "**can't get spam info**"
	for _, check := range resp.CheckResults {
		spamInfo = append(spamInfo, "- "+escapeMarkDownV1Text(check.String()))
	}
	if len(spamInfo) > 0 {
		spamInfoText = strings.Join(spamInfo, "\n")
	}

	heading := fmt.Sprintf("**original detection results for %q (%d)**\n\n%s",
		escapeMarkDownV1Text(username), fwdID, spamInfoText)
	ban := actionResult{Status: "not_attempted", Reason: "dry mode"}
	original := actionResult{Status: "not_attempted", Reason: "locator fallback; original message needs manual deletion"}
	if a.dry {
		return a.finishModeration(heading+moderationText(ban, original, "not scheduled"), nil)
	}
	if err := a.bot.UpdateSpam(msgTxt); err != nil {
		return a.finishModeration(heading+moderationText(
			actionResult{Status: "not_attempted", Reason: "learning failed"}, original, "not scheduled"),
			fmt.Errorf("learning_failed: %w", err))
	}
	banReq := banRequest{duration: bot.PermanentBanDuration, userID: fwdID, chatID: a.primChatID,
		tbAPI: a.tbAPI, dry: a.dry, training: a.trainingMode, userName: username}
	ban, err := banResult(banReq)
	if err != nil {
		errs = multierror.Append(errs, err)
	}
	log.Printf("[INFO] fallback moderation result: chat_id=%d user_id=%d ban=%s delete=%s",
		a.primChatID, fwdID, ban, original)
	if err := a.finishModeration(heading+moderationText(ban, original, "not scheduled (locator fallback)"),
		errs.ErrorOrNil()); err != nil {
		return err
	}

	log.Printf("[INFO] admin report handled: outcome=degraded_fallback admin_msg_id=%d target_type=%s target_id=%d",
		update.Message.MessageID, adminReportIdentityUser, fwdID)
	return nil
}

func (a *admin) enqueueForward(
	update tbapi.Update,
	identityID int64,
	msgID int,
	knownExact bool,
	learnText string,
	cleanup bool,
) error {
	if update.Message == nil || update.Message.Chat.ID == 0 {
		return fmt.Errorf("missing delivery chat; durable action blocked")
	}
	kind := "user"
	if identityID < 0 {
		kind = "sender_chat"
	}
	key := fmt.Sprintf("delivery:%d:%d", update.Message.Chat.ID, update.Message.MessageID)
	if knownExact {
		key = fmt.Sprintf("source:%d:%d:%s:%d", a.primChatID, msgID, kind, identityID)
	}
	p := moderation.Plan{
		Origin: "forward", Card: true, SourceText: learnText,
		Key:             key,
		ChatID:          a.primChatID,
		MessageID:       msgID,
		IdentityID:      identityID,
		IdentityKind:    kind,
		Destination:     a.adminChatID,
		DeliveryChat:    update.Message.Chat.ID,
		DeliveryMessage: update.Message.MessageID,
	}
	p.Origin = fmt.Sprintf("forward:%d:%d", p.DeliveryChat, p.DeliveryMessage)
	identity := adminForwardOriginIdentity(update.Message.ForwardOrigin)
	p.UserName = identity.Name
	base := moderation.Action{ChatID: a.primChatID, IdentityID: identityID, IdentityKind: kind}
	if identityID != a.primChatID {
		ban := base
		ban.Kind = "ban"
		ban.UntilDate = time.Now().Add(bot.PermanentBanDuration).Unix()
		p.Actions = append(p.Actions, ban)
	}
	if msgID > 0 {
		del := base
		del.Kind = "original_delete"
		del.MessageID = msgID
		del.MessageDate = locatorMessageDate(a.locator, a.primChatID, msgID)
		p.Actions = append(p.Actions, del)
	} else {
		del := base
		del.Kind = "original_delete"
		del.State = moderation.Manual
		del.Code = "locator_fallback_original_unlocated"
		p.Actions = append(p.Actions, del)
	}
	if cleanup && identityID != a.primChatID {
		var ids []int
		var err error
		if scoped, ok := a.locator.(interface {
			GetUserMessageIDsInChat(context.Context, int64, int64, int) ([]int, error)
		}); ok {
			ids, err = scoped.GetUserMessageIDsInChat(
				context.Background(),
				identityID,
				a.primChatID,
				a.aggressiveCleanupLimit,
			)
		} else {
			ids, err = a.locator.GetUserMessageIDs(context.Background(), identityID, a.aggressiveCleanupLimit)
		}
		if err != nil {
			item := base
			item.Kind = "cleanup_delete"
			item.State = moderation.Manual
			item.Code = "cleanup_selection_unavailable"
			p.Actions = append(p.Actions, item)
		}
		for _, id := range ids {
			if id == msgID {
				continue
			}
			del := base
			del.Kind = "cleanup_delete"
			del.MessageID = id
			del.MessageDate = locatorMessageDate(a.locator, a.primChatID, id)
			p.Actions = append(p.Actions, del)
		}
	}
	return a.submitRecovery(p, func() error {
		_ = a.bot.RemoveApprovedUser(identityID)
		if learnText != "" {
			return a.bot.UpdateSpam(learnText)
		}
		return nil
	})
}
