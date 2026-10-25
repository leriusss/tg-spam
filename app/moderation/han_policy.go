package moderation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// HanOrigin identifies only the new, separately withdrawable Han authority.
const HanOrigin = "han:v2"

// EnableHanBans records an immutable per-instance rollout boundary, not work.
// Telegram dates have second precision: only later seconds may request a Han ban.
func (j *Journal) EnableHanBans(ctx context.Context, since time.Time) error {
	_, err := j.db.ExecContext(ctx, j.db.Adopt(`INSERT INTO moderation_han_policy VALUES (?,?) ON CONFLICT(gid) DO NOTHING`),
		j.db.GID(), since.Unix())
	return journalError(err)
}

func validHanPlan(p Plan) bool {
	if p.Learn || p.DeleteOnly || p.RequesterID != 0 || p.CommandID != "" || !p.Card || p.Destination == 0 ||
		p.DeliveryMessage != 0 || p.DeliveryChat != 0 || p.Origin != "detector" || p.MessageID <= 0 {
		return false
	}
	deletes, bans := 0, 0
	for _, a := range p.HanActions {
		if a.ChatID != p.ChatID || a.IdentityID != p.IdentityID || a.IdentityKind != p.IdentityKind {
			return false
		}
		switch a.Kind {
		case "original_delete":
			if a.MessageID != p.MessageID || a.MessageDate < 0 {
				return false
			}
			deletes++
		case "ban", "restrict":
			if a.MessageID != 0 || p.IdentityID == 0 {
				return false
			}
			bans++
		default:
			return false
		}
	}
	return deletes == 1 && bans <= 1 && (p.IdentityKind != "message" || len(p.Actions) == 0)
}

func (j *Journal) prepareHanPlan(ctx context.Context, tx journalTx, p Plan, now time.Time) (Plan, error) {
	var since int64
	err := tx.GetContext(ctx, &since, j.db.Adopt(`SELECT since_date FROM moderation_han_policy WHERE gid=?`), j.db.GID())
	if err != nil && !errors.Is(err, sqlNoRows) {
		return p, journalError(err)
	}
	var date int64
	for _, a := range p.HanActions {
		if a.Kind == "original_delete" {
			date = a.MessageDate
		}
	}
	// An ancient re-delivery must not regain Han ban authority after receipt pruning.
	// Use the existing original-delete age window; do not extend either action's deadline.
	eligible := err == nil && date > since && date > 0 && date <= now.Unix() &&
		date > now.Add(-48*time.Hour+2*time.Minute).Unix()
	p.hanOrigins = make(map[string][]string)
	all := slices.Clone(p.Actions)
	for _, a := range all {
		p.hanOrigins[actionKey(p.Key, a)] = []string{p.Origin}
	}
	for _, a := range p.HanActions {
		if (a.Kind == "ban" || a.Kind == "restrict") && !eligible {
			p.Reason += "\nБлокировка по Han не запрошена: дата оригинала недостоверна, до границы политики или вне окна удаления."
			continue
		}
		key := actionKey(p.Key, a)
		p.hanOrigins[key] = append(p.hanOrigins[key], HanOrigin)
		all = append(all, a)
	}
	p.Actions = all
	return p, nil
}

// StopHanBan withdraws only Han's ban/restrict demand for one persisted receipt.
// Independent demands and the Han deletion stay active. Dispatched effects cannot
// be recalled; confirmed bans are never undone by this operation.
func (j *Journal) StopHanBan(ctx context.Context, receipt string, now time.Time) error {
	if receipt == "" {
		return fmt.Errorf("exact receipt is required")
	}
	return j.transaction(ctx, func(tx journalTx) error {
		if e := j.exec(ctx, tx, `UPDATE moderation_demands SET active=FALSE WHERE gid=? AND receipt=? AND origin=? AND action_id IN (SELECT id FROM moderation_actions WHERE gid=? AND receipt=? AND kind IN ('ban','restrict') AND state IN ('pending','running'))`, j.db.GID(), receipt, HanOrigin, j.db.GID(), receipt); e != nil {
			return e
		}
		if e := j.withdrawUnwanted(ctx, tx, receipt, now); e != nil {
			return e
		}
		if e := j.refreshReceipt(ctx, tx, receipt); e != nil {
			return e
		}
		return j.dirtyCard(ctx, tx, receipt, now)
	})
}

func (j *Journal) hanDeleteAuthority(ctx context.Context, a Action) (bool, error) {
	var n int
	err := j.db.GetContext(ctx, &n, j.db.Adopt(`SELECT count(*) FROM moderation_actions a JOIN moderation_receipts r ON r.gid=a.gid AND r.id=a.receipt JOIN moderation_demands d ON d.gid=a.gid AND d.action_id=a.id WHERE a.gid=? AND a.id=? AND a.receipt=? AND a.kind='original_delete' AND a.identity_kind='message' AND a.identity_id=0 AND a.chat_id=? AND a.message_id=? AND r.chat_id=a.chat_id AND r.message_id=a.message_id AND r.identity_kind='message' AND r.identity_id=0 AND d.origin=? AND d.active=TRUE`), j.db.GID(), a.ID, a.Receipt, a.ChatID, a.MessageID, HanOrigin)
	return n == 1, journalError(err)
}
