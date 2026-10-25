package moderation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Card keeps original context separate from presentation/statuses and exposes the SQL action view.
type Card struct {
	DeleteOnly          bool     `db:"-"`
	RequesterID         int64    `db:"-"`
	Reason              string   `db:"-"`
	RequestState        string   `db:"-"`
	RequestExpires      int64    `db:"-"`
	Ref                 string   `db:"ref"`
	Receipt             string   `db:"receipt"`
	Destination         int64    `db:"destination"`
	ChatID              int64    `db:"chat_id"`
	IdentityID          int64    `db:"identity_id"`
	IdentityKind        string   `db:"identity_kind"`
	MessageID           int      `db:"message_id"`
	Text                string   `db:"text"`
	UserName            string   `db:"user_name"`
	DisplayName         string   `db:"display_name"`
	Actions             []Action `db:"-"`
	Origins             []Demand `db:"-"`
	Held                bool     `db:"-"`
	Resolved            []string `db:"-"`
	IncompleteDecisions int      `db:"-"`
	RequestedBanKind    string   `db:"-"`
}

// Demand records independent provenance, not a second scheduling queue.
type Demand struct {
	ActionID string `db:"action_id"`
	Origin   string `db:"origin"`
	Active   bool   `db:"active"`
}

func (j *Journal) migrateLifecycle(ctx context.Context) error {
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS moderation_reasons (gid TEXT NOT NULL, receipt TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(gid,receipt))`,
		`CREATE TABLE IF NOT EXISTS moderation_demands (gid TEXT NOT NULL, receipt TEXT NOT NULL, action_id TEXT NOT NULL, origin TEXT NOT NULL, active BOOLEAN NOT NULL, accepted_at BIGINT NOT NULL, PRIMARY KEY(gid,action_id,origin))`,
		`CREATE TABLE IF NOT EXISTS moderation_cards (gid TEXT NOT NULL, receipt TEXT NOT NULL, ref TEXT NOT NULL, destination BIGINT NOT NULL, chat_id BIGINT NOT NULL, identity_id BIGINT NOT NULL, identity_kind TEXT NOT NULL, message_id INTEGER NOT NULL, text TEXT NOT NULL, user_name TEXT NOT NULL, display_name TEXT NOT NULL, PRIMARY KEY(gid,receipt), UNIQUE(gid,ref))`,
		`CREATE TABLE IF NOT EXISTS moderation_withdrawals (gid TEXT NOT NULL, receipt TEXT NOT NULL, origin TEXT NOT NULL, PRIMARY KEY(gid,receipt,origin))`,
		`CREATE TABLE IF NOT EXISTS moderation_holds (gid TEXT NOT NULL, chat_id BIGINT NOT NULL, identity_id BIGINT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY(gid,chat_id,identity_id))`,
		`CREATE TABLE IF NOT EXISTS moderation_resolutions (gid TEXT NOT NULL, action_id TEXT NOT NULL, receipt TEXT NOT NULL, operator_id BIGINT NOT NULL, closed_at BIGINT NOT NULL, PRIMARY KEY(gid,action_id))`,
		`CREATE TABLE IF NOT EXISTS moderation_decisions (gid TEXT NOT NULL, id TEXT NOT NULL, receipt TEXT NOT NULL, verb TEXT NOT NULL, operator_id BIGINT NOT NULL, state TEXT NOT NULL, PRIMARY KEY(gid,id))`,
		`CREATE INDEX IF NOT EXISTS idx_moderation_demands_receipt ON moderation_demands(gid,receipt)`,
	}
	for _, q := range ddl {
		if _, err := j.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("moderation lifecycle migration: %w", err)
		}
	}
	// Prototype jobs have unknown provenance: never guess they originated from reports.
	return j.transaction(ctx, func(tx journalTx) error {
		var missing []Action
		q := `SELECT a.id,a.receipt,a.state FROM moderation_actions a WHERE a.gid=? AND a.kind NOT IN ('notice','card') AND NOT EXISTS (SELECT 1 FROM moderation_demands d WHERE d.gid=a.gid AND d.action_id=a.id)`
		if err := tx.SelectContext(ctx, &missing, j.db.Adopt(q), j.db.GID()); err != nil {
			return journalError(err)
		}
		for _, a := range missing {
			if err := j.exec(ctx, tx, `INSERT INTO moderation_demands VALUES (?,?,?,'legacy',FALSE,0) ON CONFLICT(gid,action_id,origin) DO NOTHING`, j.db.GID(), a.Receipt, a.ID); err != nil {
				return err
			}
			if a.State == Pending || a.State == Running {
				if err := j.exec(ctx, tx, `UPDATE moderation_actions SET state='manual_review',code='legacy_provenance_unknown',uncertain=CASE WHEN state='running' THEN TRUE ELSE uncertain END,owner='',lease_until=0 WHERE gid=? AND id=?`, j.db.GID(), a.ID); err != nil {
					return err
				}
			}
			if err := j.refreshReceipt(ctx, tx, a.Receipt); err != nil {
				return err
			}
		}
		return nil
	})
}

func actionKey(receipt string, a Action) string {
	if deleteKind(a.Kind) {
		return fmt.Sprintf("%s:delete:%d", receipt, a.MessageID)
	}
	return fmt.Sprintf("%s:%s:%d", receipt, a.Kind, a.MessageID)
}

func permanent(until int64, now time.Time) bool {
	return until == 0 || until >= now.Add(366*24*time.Hour).Unix()
}

func (j *Journal) matching(ctx context.Context, tx journalTx, p Plan, a Action, now time.Time) (Action, bool, error) {
	var rows []Action
	q := `SELECT id,kind,message_id,until_date,state,code,next_at FROM moderation_actions WHERE gid=? AND receipt=? AND (kind=? OR (? IN ('original_delete','cleanup_delete') AND kind IN ('original_delete','cleanup_delete'))) AND message_id=? ORDER BY CASE WHEN kind IN ('original_delete','cleanup_delete') AND state='confirmed' THEN 0 WHEN kind IN ('original_delete','cleanup_delete') AND state IN ('pending','running') THEN 1 ELSE 2 END,id`
	if err := tx.SelectContext(ctx, &rows, j.db.Adopt(q), j.db.GID(), p.Key, a.Kind, a.Kind, a.MessageID); err != nil {
		return Action{}, false, journalError(err)
	}
	a.ID = "" // Admission compares desired semantics; only registration uses the selected row ID.
	return j.matchRows(ctx, tx, p, a, rows, now)
}

func (j *Journal) matchRows(
	ctx context.Context, tx journalTx, p Plan, a Action, rows []Action, now time.Time,
) (Action, bool, error) {
	for _, old := range rows {
		if a.ID != "" && old.ID == a.ID {
			return old, true, nil
		}
		// a pending sender request is not an executable deletion demand.
		if deleteKind(a.Kind) && old.State == Manual && old.Code == "sender_command_pending_confirmation" &&
			a.State != Manual && p.RequesterID == 0 && !strings.HasPrefix(p.Origin, "report:sender:") {
			continue
		}
		// Explicit approval is a new demand, not an automatic retry of a terminal result.
		if (strings.HasPrefix(p.Origin, "operator:") || strings.HasPrefix(p.Origin, "report:approval:")) &&
			old.State != Pending && old.State != Running && old.State != Confirmed {
			continue
		}
		if old.MessageID != a.MessageID || old.Kind != a.Kind && (!deleteKind(old.Kind) || !deleteKind(a.Kind)) {
			continue
		}
		if !deleteKind(a.Kind) && old.State == Confirmed {
			var undone int
			if err := tx.GetContext(ctx, &undone, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND identity_id=? AND kind IN ('unban','restore') AND state!='canceled' AND next_at>=?`), j.db.GID(), a.ChatID, a.IdentityID, old.NextAt); err != nil {
				return Action{}, false, journalError(err)
			}
			if undone > 0 {
				continue
			}
		}
		if deleteKind(a.Kind) {
			return old, true, nil
		}
		if permanent(old.UntilDate, now) == permanent(a.UntilDate, now) {
			if permanent(a.UntilDate, now) || old.UntilDate == a.UntilDate {
				return old, true, nil
			}
			// Repeated detector delivery cannot slide its original fixed temporary-ban expiry.
			var own int
			if err := tx.GetContext(ctx, &own, j.db.Adopt(`SELECT count(*) FROM moderation_demands WHERE gid=? AND action_id=? AND origin=?`), j.db.GID(), old.ID, p.Origin); err != nil {
				return Action{}, false, journalError(err)
			}
			if p.Origin == "detector" && own > 0 {
				return old, true, nil
			}
		}
	}
	return Action{}, false, nil
}

func (j *Journal) mergePlan(ctx context.Context, tx journalTx, p Plan, now time.Time) error {
	var queued int
	if err := tx.GetContext(ctx, &queued, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND kind NOT IN ('notice','card') AND state IN ('pending','running')`), j.db.GID()); err != nil {
		return journalError(err)
	}
	for i, a := range p.Actions {
		if a.ChatID != p.ChatID || a.IdentityID != p.IdentityID || a.IdentityKind != p.IdentityKind ||
			(!deleteKind(a.Kind) && a.Kind != "ban" && a.Kind != "restrict") ||
			deleteKind(a.Kind) && a.MessageID <= 0 && a.State != Manual {
			return errors.New("invalid appended action authority")
		}
		matched, exists, err := j.matching(ctx, tx, p, a, now)
		if err != nil {
			return err
		}
		if exists {
			p.Actions[i].ID = matched.ID
			continue
		}
		a.ID = actionKey(p.Key, a)
		var collision int
		if err = tx.GetContext(ctx, &collision, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND id=?`), j.db.GID(), a.ID); err != nil {
			return journalError(err)
		}
		if collision > 0 {
			a.ID += ":" + token()
		}
		p.Actions[i].ID = a.ID
		a.Receipt = p.Key
		a.NextAt = now.UnixMilli()
		a.Deadline = now.Add(24 * time.Hour).UnixMilli()
		if a.State != Manual {
			a.State = Pending
		}
		if deleteKind(a.Kind) {
			if a.MessageDate > 0 && a.MessageDate <= now.Unix() {
				a.Deadline = min(a.Deadline, time.Unix(a.MessageDate, 0).Add(48*time.Hour-2*time.Minute).UnixMilli())
			} else {
				a.MessageDate = 0
			}
		}
		if (a.Kind == "ban" || a.Kind == "restrict") && !permanent(a.UntilDate, now) {
			a.Deadline = min(a.Deadline, time.Unix(a.UntilDate, 0).Add(-30*time.Second).UnixMilli())
		}
		if queued >= j.limits.Actions {
			if a.Kind != "cleanup_delete" {
				a.State = Manual
				a.Code = "append_capacity_manual_required"
			}
			a.State = Manual
			if a.Kind == "cleanup_delete" {
				a.Code = "cleanup_capacity_manual_required"
			}
		}
		if a.State == Pending && !now.Add(15*time.Second).Before(time.UnixMilli(a.Deadline)) {
			a.State = Expired
			a.Code = "deadline_manual_action_required"
		}
		if a.State == Pending {
			queued++
		}
		if insertErr := j.insertAction(ctx, tx, a); insertErr != nil {
			return insertErr
		}
	}
	return nil
}

func (j *Journal) registerPlan(ctx context.Context, tx journalTx, p Plan, now time.Time) error {
	changed := false
	var withdrawn int
	if err := tx.GetContext(ctx, &withdrawn, j.db.Adopt(`SELECT count(*) FROM moderation_withdrawals WHERE gid=? AND receipt=? AND origin=?`), j.db.GID(), p.Key, p.Origin); err != nil {
		return journalError(err)
	}
	var held int
	if err := tx.GetContext(ctx, &held, j.db.Adopt(`SELECT count(*) FROM moderation_holds WHERE gid=? AND chat_id=? AND identity_id=? AND created_at>=?`), j.db.GID(), p.ChatID, p.IdentityID, now.UnixMilli()); err != nil {
		return journalError(err)
	}
	var rows []Action
	if err := tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT id,kind,message_id,until_date,state,code,next_at FROM moderation_actions WHERE gid=? AND receipt=? ORDER BY CASE WHEN kind IN ('original_delete','cleanup_delete') AND state='confirmed' THEN 0 WHEN kind IN ('original_delete','cleanup_delete') AND state IN ('pending','running') THEN 1 ELSE 2 END,id`), j.db.GID(), p.Key); err != nil {
		return journalError(err)
	}
	withdrawals := map[string]int{p.Origin: withdrawn}
	for _, a := range p.Actions {
		old, ok, err := j.matchRows(ctx, tx, p, a, rows, now)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("registered demand action is missing")
		}
		origins := []string{p.Origin}
		if p.hanOrigins != nil {
			origins = p.hanOrigins[actionKey(p.Key, a)]
		}
		for _, origin := range origins {
			if _, known := withdrawals[origin]; !known {
				var count int
				if e := tx.GetContext(ctx, &count, j.db.Adopt(`SELECT count(*) FROM moderation_withdrawals WHERE gid=? AND receipt=? AND origin=?`), j.db.GID(), p.Key, origin); e != nil {
					return journalError(e)
				}
				withdrawals[origin] = count
			}
			res, e := tx.ExecContext(
				ctx,
				j.db.Adopt(`INSERT INTO moderation_demands VALUES (?,?,?,?,?,?) ON CONFLICT(gid,action_id,origin) DO NOTHING`),
				j.db.GID(),
				p.Key,
				old.ID,
				origin,
				withdrawals[origin] == 0 && held == 0, now.UnixMilli(),
			)
			if e != nil {
				return journalError(e)
			}
			n, e := res.RowsAffected()
			if e != nil {
				return journalError(e)
			}
			changed = changed || n > 0
		}
	}
	if p.Reason != "" {
		var prior []string
		reasonErr := tx.SelectContext(ctx, &prior,
			j.db.Adopt(`SELECT reason FROM moderation_reasons WHERE gid=? AND receipt=?`), j.db.GID(), p.Key)
		if reasonErr != nil {
			return journalError(reasonErr)
		}
		merged := ""
		if len(prior) > 0 {
			merged = prior[0]
		}
		for line := range strings.SplitSeq(p.Reason, "\n") {
			if line != "" && !slices.Contains(strings.Split(merged, "\n"), line) {
				if merged != "" {
					merged += "\n"
				}
				merged += line
			}
		}
		if len(prior) == 0 || merged != prior[0] {
			reasonErr = j.exec(ctx, tx, `INSERT INTO moderation_reasons VALUES (?,?,?) ON CONFLICT(gid,receipt) DO UPDATE SET reason=excluded.reason`,
				j.db.GID(), p.Key, merged)
			if reasonErr != nil {
				return reasonErr
			}
			changed = true
		}
	}
	if p.Card {
		res, e := tx.ExecContext(
			ctx,
			j.db.Adopt(`INSERT INTO moderation_cards VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(gid,receipt) DO NOTHING`),
			j.db.GID(),
			p.Key,
			token(),
			p.Destination,
			p.ChatID,
			p.IdentityID,
			p.IdentityKind,
			p.MessageID,
			p.SourceText,
			p.UserName,
			p.DisplayName,
		)
		if e != nil {
			return journalError(e)
		}
		n, e := res.RowsAffected()
		if e != nil {
			return journalError(e)
		}
		changed = changed || n > 0
	}
	if err := j.withdrawUnwanted(ctx, tx, p.Key, now); err != nil {
		return err
	}
	// An independent newly active authority can restore known-unsent canceled work.
	if err := j.exec(ctx, tx, `UPDATE moderation_actions SET state='pending',code='',attempts=0,next_at=?,deadline=CASE WHEN kind IN ('original_delete','cleanup_delete') AND message_date>0 AND message_date*1000+172680000<? THEN message_date*1000+172680000 WHEN kind IN ('ban','restrict') AND until_date>0 AND until_date*1000-30000<? THEN until_date*1000-30000 ELSE ? END WHERE gid=? AND receipt=? AND state='canceled' AND EXISTS (SELECT 1 FROM moderation_demands d WHERE d.gid=moderation_actions.gid AND d.action_id=moderation_actions.id AND d.active=TRUE)`, now.UnixMilli(), now.Add(24*time.Hour).UnixMilli(), now.Add(24*time.Hour).UnixMilli(), now.Add(24*time.Hour).UnixMilli(), j.db.GID(), p.Key); err != nil {
		return err
	}
	// Reactivate only receipt scheduling, never an already terminal failed action.
	if err := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='ready' WHERE gid=? AND id=? AND state='finished' AND EXISTS (SELECT 1 FROM moderation_actions WHERE gid=? AND receipt=? AND state IN ('pending','running'))`, j.db.GID(), p.Key, j.db.GID(), p.Key); err != nil {
		return err
	}
	if err := j.refreshReceipt(ctx, tx, p.Key); err != nil {
		return err
	}
	if changed {
		return j.dirtyCard(ctx, tx, p.Key, now)
	}
	return nil
}

func (j *Journal) dirtyCard(ctx context.Context, tx journalTx, receipt string, now time.Time) error {
	var card Card
	err := tx.GetContext(
		ctx,
		&card,
		j.db.Adopt(`SELECT ref,receipt,destination FROM moderation_cards WHERE gid=? AND receipt=?`),
		j.db.GID(),
		receipt,
	)
	if errors.Is(err, sqlNoRows) {
		return nil
	}
	if err != nil {
		return journalError(err)
	}
	id := receipt + ":card"
	a := Action{
		ID:           id,
		Receipt:      receipt,
		Kind:         "card",
		ChatID:       card.Destination,
		IdentityKind: "system",
		State:        Pending,
		NextAt:       now.UnixMilli(),
		Deadline:     now.Add(24 * time.Hour).UnixMilli(),
	}
	var found int
	if err = tx.GetContext(ctx, &found, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND id=?`), j.db.GID(), id); err != nil {
		return journalError(err)
	}
	if found == 0 {
		if insertErr := j.insertAction(ctx, tx, a); insertErr != nil {
			return insertErr
		}
	}
	return j.exec(
		ctx,
		tx,
		`UPDATE moderation_actions SET generation=generation+1,deadline=?,attempts=CASE WHEN state='running' THEN attempts ELSE 0 END,state=CASE WHEN state='running' OR (state='notification_undelivered' AND code IN ('telegram_400','telegram_401','telegram_403','telegram_404','telegram_501','notification_destination_changed')) THEN state ELSE 'pending' END WHERE gid=? AND id=?`,
		a.Deadline,
		j.db.GID(),
		id,
	)
}

// Allowed is rechecked immediately before mutation; cancellation is not a Telegram request fence.
func (j *Journal) Allowed(ctx context.Context, a Action) (bool, error) {
	mode, err := j.Authorize(ctx, a)
	return mode == "allowed", err
}

// Authorize distinguishes withdrawn authority from a new requirement waiting for an operator undo.
func (j *Journal) Authorize(ctx context.Context, a Action) (string, error) {
	var count int
	q := `SELECT count(*) FROM moderation_demands d WHERE d.gid=? AND d.action_id=? AND d.active=TRUE AND (? IN ('unban','restore') OR NOT EXISTS (SELECT 1 FROM moderation_holds h WHERE h.gid=d.gid AND h.chat_id=? AND h.identity_id=? AND h.created_at>=d.accepted_at))`
	if err := j.db.GetContext(ctx, &count, j.db.Adopt(q), j.db.GID(), a.ID, a.Kind, a.ChatID, a.IdentityID); err != nil {
		return "", journalError(err)
	}
	if count == 0 {
		return "withdrawn", nil
	}
	if a.Kind == "unban" || a.Kind == "restore" {
		return "allowed", nil
	}
	q = `SELECT (SELECT count(*) FROM moderation_actions u WHERE u.gid=? AND u.chat_id=? AND u.identity_id=? AND u.kind IN ('unban','restore') AND u.state NOT IN ('confirmed','canceled') AND NOT EXISTS (SELECT 1 FROM moderation_resolutions r WHERE r.gid=u.gid AND r.action_id=u.id))+(SELECT count(*) FROM moderation_decisions d JOIN moderation_receipts r ON d.gid=r.gid AND d.receipt=r.id WHERE d.gid=? AND r.chat_id=? AND r.identity_id=? AND d.verb IN ('unban','restore') AND d.state='started')`
	if err := j.db.GetContext(ctx, &count, j.db.Adopt(q), j.db.GID(), a.ChatID, a.IdentityID,
		j.db.GID(), a.ChatID, a.IdentityID); err != nil {
		return "", journalError(err)
	}
	if count > 0 {
		return "waiting_for_undo", nil
	}
	// Callback preparation does not pause independently ready detector/forward authority.
	q = `SELECT count(*) FROM moderation_decisions d WHERE d.gid=? AND d.receipt=? AND d.verb IN ('approve','approve_report') AND d.state='started' AND NOT EXISTS (SELECT 1 FROM moderation_demands independent WHERE independent.gid=d.gid AND independent.action_id=? AND independent.active=TRUE AND independent.origin NOT IN ('report','operator') AND independent.origin NOT LIKE 'report:%' AND independent.origin NOT LIKE 'operator:%')`
	if err := j.db.GetContext(ctx, &count, j.db.Adopt(q), j.db.GID(), a.Receipt, a.ID); err != nil {
		return "", journalError(err)
	}
	if count > 0 {
		return "waiting_for_operator", nil
	}
	return "allowed", nil
}

func (j *Journal) finishWithdrawal(ctx context.Context, tx journalTx, a Action, o *Outcome, _ time.Time) error {
	if a.Kind == "notice" || a.Kind == "card" {
		return nil
	}
	var active int
	if err := tx.GetContext(ctx, &active, j.db.Adopt(`SELECT count(*) FROM moderation_demands WHERE gid=? AND action_id=? AND active=TRUE`), j.db.GID(), a.ID); err != nil {
		return journalError(err)
	}
	if active == 0 && (o.Retry || o.State == Canceled && (o.Uncertain || a.Uncertain)) {
		o.Retry = false
		if o.Uncertain || a.Uncertain {
			o.State = Manual
			o.Code = "withdrawn_request_outcome_unknown"
			o.Uncertain = true
		} else {
			o.State = Canceled
			o.Code = "withdrawn_before_retry"
		}
	}
	return nil
}

func (j *Journal) withdrawUnwanted(ctx context.Context, tx journalTx, receipt string, _ time.Time) error {
	return j.exec(
		ctx,
		tx,
		`UPDATE moderation_actions SET state=CASE WHEN uncertain=TRUE THEN 'manual_review' ELSE 'canceled' END,code=CASE WHEN uncertain=TRUE THEN 'withdrawn_request_outcome_unknown' ELSE 'withdrawn_before_execution' END WHERE gid=? AND receipt=? AND (state='pending' OR (state='manual_review' AND code IN ('manual_report_pending_decision','legacy_context_only','sender_command_pending_confirmation'))) AND kind NOT IN ('notice','card') AND NOT EXISTS (SELECT 1 FROM moderation_demands d WHERE d.gid=moderation_actions.gid AND d.action_id=moderation_actions.id AND d.active=TRUE)`,
		j.db.GID(),
		receipt,
	)
}

// Withdraw removes report authority only; detector/forward decisions remain independently active.
func (j *Journal) Withdraw(ctx context.Context, receipt, origin string, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error { return j.withdrawTX(ctx, tx, receipt, origin, now) })
}
func (j *Journal) withdrawTX(ctx context.Context, tx journalTx, receipt, origin string, now time.Time) error {
	if origin == "report" {
		if err := j.exec(ctx, tx, `INSERT INTO moderation_withdrawals SELECT gid,receipt,origin FROM moderation_demands WHERE gid=? AND receipt=? AND (origin='report' OR origin LIKE 'report:%') ON CONFLICT(gid,receipt,origin) DO NOTHING`, j.db.GID(), receipt); err != nil {
			return err
		}
	}
	if err := j.exec(ctx, tx, `INSERT INTO moderation_withdrawals VALUES (?,?,?) ON CONFLICT(gid,receipt,origin) DO NOTHING`, j.db.GID(), receipt, origin); err != nil {
		return err
	}
	if err := j.exec(ctx, tx, `UPDATE moderation_demands SET active=FALSE WHERE gid=? AND receipt=? AND (origin=? OR (?='report' AND origin LIKE 'report:%'))`, j.db.GID(), receipt, origin, origin); err != nil {
		return err
	}
	if err := j.withdrawUnwanted(ctx, tx, receipt, now); err != nil {
		return err
	}
	if err := j.refreshReceipt(ctx, tx, receipt); err != nil {
		return err
	}
	return j.dirtyCard(ctx, tx, receipt, now)
}

// CardView loads original context, not a text reconstructed from Telegram notification statuses.
func (j *Journal) CardView(ctx context.Context, ref string) (Card, error) {
	var c Card
	err := j.transaction(ctx, func(tx journalTx) error {
		err := tx.GetContext(
			ctx,
			&c,
			j.db.Adopt(
				`SELECT ref,receipt,destination,chat_id,identity_id,identity_kind,message_id,text,user_name,display_name FROM moderation_cards WHERE gid=? AND (ref=? OR receipt=?)`,
			),
			j.db.GID(),
			ref,
			ref,
		)
		if err != nil {
			return journalError(err)
		}
		err = tx.SelectContext(
			ctx,
			&c.Actions,
			j.db.Adopt(
				`SELECT id,receipt,kind,chat_id,identity_id,identity_kind,message_id,message_date,until_date,state,code,attempts,deadline,next_at,owner,lease_until,uncertain,generation,delivered,notification_id FROM moderation_actions WHERE gid=? AND receipt=? AND kind!='card' ORDER BY id`,
			),
			j.db.GID(),
			c.Receipt,
		)
		if err != nil {
			return journalError(err)
		}
		err = tx.SelectContext(
			ctx,
			&c.Origins,
			j.db.Adopt(
				`SELECT action_id,origin,active FROM moderation_demands WHERE gid=? AND receipt=? ORDER BY origin,action_id`,
			),
			j.db.GID(),
			c.Receipt,
		)
		if err != nil {
			return journalError(err)
		}
		var deleteRequests []string
		if e := tx.SelectContext(ctx, &deleteRequests, j.db.Adopt(`SELECT receipt FROM moderation_delete_requests WHERE gid=? AND receipt=?`), j.db.GID(), c.Receipt); e != nil {
			return journalError(e)
		}
		c.DeleteOnly = len(deleteRequests) > 0
		var reasons []string
		if reasonErr := tx.SelectContext(ctx, &reasons, j.db.Adopt(`SELECT reason FROM moderation_reasons WHERE gid=? AND receipt=?`),
			j.db.GID(), c.Receipt); reasonErr != nil {
			return journalError(reasonErr)
		}
		if len(reasons) > 0 {
			c.Reason = reasons[0]
		}
		var held int
		err = tx.GetContext(
			ctx,
			&held,
			j.db.Adopt(
				`SELECT count(*) FROM moderation_holds h WHERE gid=? AND chat_id=? AND identity_id=? AND NOT EXISTS (SELECT 1 FROM moderation_demands d JOIN moderation_actions a ON d.gid=a.gid AND d.action_id=a.id WHERE d.gid=h.gid AND d.receipt=? AND d.active=TRUE AND d.accepted_at>h.created_at AND a.kind NOT IN ('unban','restore'))`,
			),
			j.db.GID(),
			c.ChatID,
			c.IdentityID,
			c.Receipt,
		)
		if err == nil {
			err = tx.SelectContext(
				ctx,
				&c.Resolved,
				j.db.Adopt(`SELECT action_id FROM moderation_resolutions WHERE gid=? AND receipt=?`),
				j.db.GID(),
				c.Receipt,
			)
		}
		if err == nil {
			err = tx.GetContext(
				ctx,
				&c.IncompleteDecisions,
				j.db.Adopt(`SELECT count(*) FROM moderation_decisions WHERE gid=? AND receipt=? AND state='started'`),
				j.db.GID(),
				c.Receipt,
			)
		}
		if err == nil {
			var requests []struct {
				State     string `db:"state"`
				Requester int64  `db:"requester"`
				Expires   int64  `db:"expires_at"`
			}
			err = tx.SelectContext(ctx, &requests, j.db.Adopt(`SELECT state,expires_at,requester FROM moderation_requests WHERE gid=? AND receipt=?`), j.db.GID(), c.Receipt)
			if len(requests) > 0 {
				c.RequesterID = requests[0].Requester
				c.RequestState = requests[0].State
				c.RequestExpires = requests[0].Expires
				if c.RequestState == "pending" && c.RequestExpires <= time.Now().UnixMilli() {
					c.RequestState = "expired"
				}
			}
		}
		c.Held = held > 0
		return journalError(err)
	})
	return c, journalError(err)
}

// BeginDecision durably claims a callback before non-replayable local learning.
func (j *Journal) BeginDecision(ctx context.Context, id, receipt, verb string, operator int64) (bool, error) {
	fresh := false
	err := j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		result, e := tx.ExecContext(
			ctx,
			j.db.Adopt(`INSERT INTO moderation_decisions VALUES (?,?,?,?,?,?) ON CONFLICT(gid,id) DO NOTHING`),
			j.db.GID(),
			id,
			receipt,
			verb,
			operator,
			"started",
		)
		if e != nil {
			return journalError(e)
		}
		n, e := result.RowsAffected()
		fresh = n == 1
		return journalError(e)
	})
	return fresh, err
}

// EndDecision never marks moderation performed; it records local callback handling only.
func (j *Journal) EndDecision(ctx context.Context, id string) error {
	return j.transaction(ctx, func(tx journalTx) error {
		var receipt string
		if err := tx.GetContext(ctx, &receipt, j.db.Adopt(`SELECT receipt FROM moderation_decisions WHERE gid=? AND id=?`), j.db.GID(), id); err != nil {
			return journalError(err)
		}
		if err := j.exec(ctx, tx, `UPDATE moderation_decisions SET state='done' WHERE gid=? AND id=?`, j.db.GID(), id); err != nil {
			return err
		}
		return j.dirtyCard(ctx, tx, receipt, time.Now())
	})
}

// Close resolves terminal manual work explicitly. Running or pending work cannot be closed.
func (j *Journal) Close(ctx context.Context, receipt string, operator int64, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error { return j.closeTX(ctx, tx, receipt, operator, now) })
}
func (j *Journal) closeTX(ctx context.Context, tx journalTx, receipt string, operator int64, now time.Time) error {
	var live int
	if err := tx.GetContext(ctx, &live, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND receipt=? AND kind NOT IN ('notice','card') AND state IN ('pending','running')`), j.db.GID(), receipt); err != nil {
		return journalError(err)
	}
	if live > 0 {
		return errors.New("moderation still pending/running; manual closure refused")
	}
	if err := j.exec(ctx, tx, `INSERT INTO moderation_resolutions SELECT gid,id,receipt,?,? FROM moderation_actions WHERE gid=? AND receipt=? AND kind NOT IN ('notice','card') ON CONFLICT(gid,action_id) DO NOTHING`, operator, now.UnixMilli(), j.db.GID(), receipt); err != nil {
		return err
	}
	if err := j.exec(ctx, tx, `UPDATE moderation_decisions SET state='closed_by_operator' WHERE gid=? AND receipt=? AND state='started'`, j.db.GID(), receipt); err != nil {
		return err
	}
	if err := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='finished' WHERE gid=? AND id=?`, j.db.GID(), receipt); err != nil {
		return err
	}
	return j.dirtyCard(ctx, tx, receipt, now)
}

// StopTarget withdraws earlier authorities before unban, including across restart.
// Later independent originals retain their policy; explicit approval/trust still gates them.
// Running effects remain recorded; an unban waits behind all running bans for the same target.
func (j *Journal) StopTarget(ctx context.Context, c Card, unbanKind string, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error { return j.stopTargetTX(ctx, tx, c, unbanKind, token(), now) })
}
func (j *Journal) stopTargetTX(ctx context.Context, tx journalTx, c Card, unbanKind, decisionID string, now time.Time) error {
	if err := j.exec(ctx, tx, `INSERT INTO moderation_holds VALUES (?,?,?,?) ON CONFLICT(gid,chat_id,identity_id) DO NOTHING`, j.db.GID(), c.ChatID, c.IdentityID, now.UnixMilli()); err != nil {
		return err
	}
	var receipts []string
	if err := tx.SelectContext(ctx, &receipts, j.db.Adopt(`SELECT id FROM moderation_receipts WHERE gid=? AND chat_id=? AND identity_id=?`), j.db.GID(), c.ChatID, c.IdentityID); err != nil {
		return journalError(err)
	}
	for _, receipt := range receipts {
		if err := j.exec(ctx, tx, `INSERT INTO moderation_withdrawals SELECT gid,receipt,origin FROM moderation_demands WHERE gid=? AND receipt=? AND active=TRUE AND action_id NOT IN (SELECT id FROM moderation_actions WHERE gid=? AND kind IN ('unban','restore')) ON CONFLICT(gid,receipt,origin) DO NOTHING`, j.db.GID(), receipt, j.db.GID()); err != nil {
			return err
		}
		if err := j.exec(ctx, tx, `UPDATE moderation_demands SET active=FALSE WHERE gid=? AND receipt=? AND action_id NOT IN (SELECT id FROM moderation_actions WHERE gid=? AND kind IN ('unban','restore'))`, j.db.GID(), receipt, j.db.GID()); err != nil {
			return err
		}
		if err := j.withdrawUnwanted(ctx, tx, receipt, now); err != nil {
			return err
		}
		if err := j.dirtyCard(ctx, tx, receipt, now); err != nil {
			return err
		}
	}
	if unbanKind != "" {
		if unbanKind != "unban" && unbanKind != "restore" {
			return errors.New("invalid undo action")
		}
		id := c.Receipt + ":" + unbanKind + ":" + decisionID
		var exists int
		if err := tx.GetContext(ctx, &exists, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND id=?`), j.db.GID(), id); err != nil {
			return journalError(err)
		}
		if exists == 0 {
			var queued int
			if err := tx.GetContext(ctx, &queued, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND kind NOT IN ('notice','card') AND state IN ('pending','running')`), j.db.GID()); err != nil {
				return journalError(err)
			}
			if queued >= j.limits.Actions {
				return ErrCapacity
			}
			a := Action{
				ID:           id,
				Receipt:      c.Receipt,
				Kind:         unbanKind,
				ChatID:       c.ChatID,
				IdentityID:   c.IdentityID,
				IdentityKind: c.IdentityKind,
				State:        Pending,
				NextAt:       now.UnixMilli(),
				Deadline:     now.Add(24 * time.Hour).UnixMilli(),
			}
			if err := j.insertAction(ctx, tx, a); err != nil {
				return err
			}
			if err := j.exec(ctx, tx, `INSERT INTO moderation_demands VALUES (?,?,?,?,TRUE,?)`, j.db.GID(), c.Receipt, id, "operator", now.UnixMilli()); err != nil {
				return err
			}
		}
	}
	if err := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='ready' WHERE gid=? AND id=?`, j.db.GID(), c.Receipt); err != nil {
		return err
	}
	if err := j.refreshReceipt(ctx, tx, c.Receipt); err != nil {
		return err
	}
	return j.dirtyCard(ctx, tx, c.Receipt, now)
}

// Decide commits callback identity and its authority changes together; learning is never replayed.
func (j *Journal) Decide(ctx context.Context, id string, c Card, verb string, operator int64, now time.Time) (bool, error) {
	fresh := false
	err := j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		var deleteOnly int
		if e := tx.GetContext(ctx, &deleteOnly, j.db.Adopt(`SELECT count(*) FROM moderation_delete_requests WHERE gid=? AND receipt=?`), j.db.GID(), c.Receipt); e != nil {
			return journalError(e)
		}
		if deleteOnly > 0 {
			return errors.New("delete-only card requires its dedicated decision")
		}
		request, allowed, requestErr := j.requestDecision(ctx, tx, c, verb, now)
		if requestErr != nil {
			return requestErr
		}
		if request && !allowed {
			return nil
		}
		result, e := tx.ExecContext(
			ctx,
			j.db.Adopt(`INSERT INTO moderation_decisions VALUES (?,?,?,?,?,?) ON CONFLICT(gid,id) DO NOTHING`),
			j.db.GID(),
			id,
			c.Receipt,
			verb,
			operator,
			"started",
		)
		if e != nil {
			return journalError(e)
		}
		n, e := result.RowsAffected()
		if e != nil {
			return journalError(e)
		}
		if n == 0 {
			return nil
		}
		fresh = true
		switch verb {
		case "reject_report":
			return j.withdrawTX(ctx, tx, c.Receipt, "report", now)
		case "cancel":
			return j.stopTargetTX(ctx, tx, c, "", id, now)
		case "unban", "restore":
			// A new explicit undo supersedes terminal undo work, preserving its API outcome.
			if resolutionErr := j.exec(ctx, tx, `INSERT INTO moderation_resolutions SELECT gid,id,receipt,?,? FROM moderation_actions WHERE gid=? AND chat_id=? AND identity_id=? AND kind IN ('unban','restore') AND state IN ('failed','manual_review','expired') ON CONFLICT(gid,action_id) DO NOTHING`, operator, now.UnixMilli(), j.db.GID(), c.ChatID, c.IdentityID); resolutionErr != nil {
				return resolutionErr
			}
			return j.stopTargetTX(ctx, tx, c, verb, id, now)
		case "close":
			return j.closeTX(ctx, tx, c.Receipt, operator, now)
		case "approve", "approve_report":
			var unsettled int
			if e = tx.GetContext(ctx, &unsettled, j.db.Adopt(`SELECT count(*) FROM moderation_actions u WHERE gid=? AND chat_id=? AND identity_id=? AND kind IN ('unban','restore') AND state NOT IN ('confirmed','canceled') AND NOT EXISTS (SELECT 1 FROM moderation_resolutions r WHERE r.gid=u.gid AND r.action_id=u.id)`), j.db.GID(), c.ChatID, c.IdentityID); e != nil {
				return journalError(e)
			}
			if unsettled > 0 {
				return errors.New("operator undo is unfinished; approval must wait for resolution")
			}
			origin := "operator:" + id
			if verb == "approve_report" {
				origin = "report:approval:" + id
			}

			kind := "ban"
			for _, a := range c.Actions {
				if a.Kind == "restrict" {
					kind = "restrict"
				}
			}
			if c.RequestedBanKind != "" {
				if c.RequestedBanKind != "ban" && c.RequestedBanKind != "restrict" {
					return errors.New("invalid requested ban policy")
				}
				kind = c.RequestedBanKind
			}
			p := Plan{
				Key:          c.Receipt,
				ChatID:       c.ChatID,
				IdentityID:   c.IdentityID,
				IdentityKind: c.IdentityKind,
				MessageID:    c.MessageID,
				Destination:  c.Destination,
				Origin:       origin,
			}
			p.Actions = []Action{
				{
					Kind:         kind,
					ChatID:       c.ChatID,
					IdentityID:   c.IdentityID,
					IdentityKind: c.IdentityKind,
					UntilDate:    now.Add(9999 * time.Hour).Unix(),
				},
			}
			if c.MessageID > 0 {
				date := int64(0)
				for _, a := range c.Actions {
					if a.MessageID == c.MessageID && deleteKind(a.Kind) {
						date = a.MessageDate
					}
				}
				p.Actions = append(
					p.Actions,
					Action{
						Kind:         "original_delete",
						ChatID:       c.ChatID,
						IdentityID:   c.IdentityID,
						IdentityKind: c.IdentityKind,
						MessageID:    c.MessageID,
						MessageDate:  date,
					},
				)
			}
			if request {
				for _, item := range c.Actions {
					if item.Kind == "cleanup_delete" && item.State == Manual {
						item.State = ""
						item.ID = ""
						p.Actions = append(p.Actions, item)
					}
				}
			}
			if operationErr := j.mergePlan(ctx, tx, p, now); operationErr != nil {
				return operationErr
			}
			if operationErr := j.registerPlan(ctx, tx, p, now); operationErr != nil {
				return operationErr
			}
			return j.exec(
				ctx,
				tx,
				`UPDATE moderation_receipts SET state='ready' WHERE gid=? AND id=?`,
				j.db.GID(),
				c.Receipt,
			)
		default:
			return errors.New("unsupported moderation decision")
		}
	})
	return fresh, err
}

// BindCard adopts a pre-existing report/legacy card; it never declares API success.
func (j *Journal) BindCard(ctx context.Context, receipt string, messageID int) error {
	_, err := j.db.ExecContext(
		ctx,
		j.db.Adopt(
			`UPDATE moderation_actions SET notification_id=? WHERE gid=? AND receipt=? AND kind='card' AND notification_id=0`,
		),
		messageID,
		j.db.GID(),
		receipt,
	)
	return journalError(err)
}

// CardBound prevents forged callbacks against another chat/card/identity.
func (j *Journal) CardBound(ctx context.Context, c Card, destination int64, messageID int) (bool, error) {
	var n int
	err := j.db.GetContext(
		ctx,
		&n,
		j.db.Adopt(
			`SELECT count(*) FROM moderation_actions WHERE gid=? AND receipt=? AND kind='card' AND chat_id=? AND notification_id=? AND notification_id>0`,
		),
		j.db.GID(),
		c.Receipt,
		destination,
		messageID,
	)
	return n == 1, journalError(err)
}

func (j *Journal) refreshReceipt(ctx context.Context, tx journalTx, receipt string) error {
	return j.exec(
		ctx,
		tx,
		`UPDATE moderation_receipts SET state='finished' WHERE gid=? AND id=? AND state='ready' AND NOT EXISTS (SELECT 1 FROM moderation_actions WHERE gid=? AND receipt=? AND kind NOT IN ('notice','card') AND state IN ('pending','running'))`,
		j.db.GID(),
		receipt,
		j.db.GID(),
		receipt,
	)
}
