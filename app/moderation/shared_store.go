package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// SharedStore owns only coordination metadata in a separate database. It never
// opens participant runtime databases or executes Telegram moderation requests.
type SharedStore struct {
	journal *Journal
	owner   int64
	peers   []int64
}

// SharedChoice freezes one local receipt's selected indices.
type SharedChoice struct {
	Report    SharedReport
	Selected  []int
	Refusal   string
	DecidedAt int64
}

// SharedIntent preserves superseded dispatch authority without replaying it.
type SharedIntent struct {
	Choices             []SharedIntentChoice
	Operator, DecidedAt int64
	Automatic           bool
	Reason              string
}

// SharedIntentChoice retains history without duplicating whole reports.
type SharedIntentChoice struct {
	Peer       int64
	Receipt    string
	MessageIDs []int
}

// SharedCase retains incoming deliveries separately from exact original targets.
type SharedCase struct {
	Policy                                            string
	Ref                                               string
	Canonical                                         string
	CanonicalAnchor                                   int
	AdminID, Operator, Created, Expires, CollectUntil int64
	Revision                                          int
	Reports                                           []SharedReport
	Choices                                           []SharedChoice
	PriorIntents                                      []SharedIntent
	Automatic, NeedsReview                            bool
	ReviewReason                                      string
	DecidedAt                                         int64
	Decided                                           bool
	DecisionOperator                                  int64
	NotificationID                                    int
	SendState, Lease, LastRender                      string
	LeaseUntil, NextAt                                int64
	Attempts, Page                                    int
}

// NewSharedStore requires an identical fixed owner/roster across participants.
func NewSharedStore(ctx context.Context, db *engine.SQL, owner int64, peers []int64) (*SharedStore, error) {
	peers = slices.Clone(peers)
	slices.Sort(peers)
	if owner <= 0 || len(peers) == 0 || len(peers) > 5 || !slices.Contains(peers, owner) {
		return nil, errors.New("invalid shared roster")
	}
	for i, p := range peers {
		if p <= 0 || i > 0 && p == peers[i-1] {
			return nil, errors.New("duplicate shared peer")
		}
	}
	j, err := NewJournal(ctx, db, DefaultLimits())
	if err != nil {
		return nil, err
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS shared_config (gid TEXT PRIMARY KEY, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS shared_cards (gid TEXT NOT NULL, ref TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(gid,ref))`,
		`CREATE TABLE IF NOT EXISTS shared_views (gid TEXT NOT NULL, destination BIGINT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(gid,destination))`,
		`CREATE TABLE IF NOT EXISTS shared_cases (gid TEXT NOT NULL, ref TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(gid,ref))`,
		`CREATE TABLE IF NOT EXISTS shared_searches (gid TEXT NOT NULL, peer BIGINT NOT NULL, destination BIGINT NOT NULL, message_id INTEGER NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(gid,peer,destination,message_id))`,
		`CREATE TABLE IF NOT EXISTS shared_deliveries (gid TEXT NOT NULL, peer BIGINT NOT NULL, update_id BIGINT NOT NULL, destination BIGINT NOT NULL, message_id INTEGER NOT NULL, operator_id BIGINT NOT NULL, ref TEXT NOT NULL, PRIMARY KEY(gid,peer,update_id), UNIQUE(gid,peer,destination,message_id))`,
	} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			return nil, journalError(err)
		}
	}
	s := &SharedStore{journal: j, owner: owner, peers: peers}
	raw, err := json.Marshal(struct {
		Owner int64
		Peers []int64
	}{owner, peers})
	if err != nil {
		return nil, journalError(err)
	}
	err = j.transaction(ctx, func(tx journalTx) error {
		if e := j.exec(ctx, tx, `INSERT INTO shared_config VALUES (?,?) ON CONFLICT(gid) DO NOTHING`, db.GID(), string(raw)); e != nil {
			return e
		}
		var saved string
		if e := tx.GetContext(ctx, &saved, db.Adopt(`SELECT payload FROM shared_config WHERE gid=?`), db.GID()); e != nil {
			return journalError(e)
		}
		if saved != string(raw) {
			return errors.New("shared roster or card owner changed; explicit migration required")
		}
		return nil
	})
	return s, err
}

func (s *SharedStore) cases(ctx context.Context, tx journalTx) ([]SharedCase, error) {
	var raw []string
	j := s.journal
	if err := tx.SelectContext(ctx, &raw, j.db.Adopt(`SELECT payload FROM shared_cases WHERE gid=? ORDER BY ref LIMIT 500`), j.db.GID()); err != nil {
		return nil, journalError(err)
	}
	out := make([]SharedCase, 0, len(raw))
	for _, r := range raw {
		var c SharedCase
		if err := json.Unmarshal([]byte(r), &c); err != nil {
			return nil, journalError(err)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b SharedCase) int {
		if a.Created < b.Created {
			return -1
		}
		if a.Created > b.Created {
			return 1
		}
		return 0
	})
	return out, nil
}

func (s *SharedStore) save(ctx context.Context, tx journalTx, c SharedCase) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return journalError(err)
	}
	return s.journal.exec(ctx, tx, `INSERT INTO shared_cases VALUES (?,?,?) ON CONFLICT(gid,ref) DO UPDATE SET payload=excluded.payload`, s.journal.db.GID(), c.Ref, string(raw))
}

func sameOriginals(a, b SharedReport) bool {
	if a.Peer != b.Peer || a.ChatID != b.ChatID || len(a.Targets) == 0 || len(a.Targets) != len(b.Targets) {
		return false
	}
	for i, t := range a.Targets {
		u := b.Targets[i]
		if t.MessageID != u.MessageID || t.IdentityKind != u.IdentityKind || t.IdentityID != u.IdentityID ||
			t.Evidence != u.Evidence || t.EvidenceUpdateID != u.EvidenceUpdateID || t.EvidenceMessageID != u.EvidenceMessageID {
			return false
		}
	}
	return true
}

func sameInstructionEpoch(a, b SharedReport) bool {
	if !sameOriginals(a, b) {
		return false
	}
	if a.Policy != ForwardAllPolicy && b.Policy != ForwardAllPolicy {
		return true
	}
	if a.Policy != b.Policy || a.Operator != b.Operator || !slices.Equal(a.Refusals, b.Refusals) {
		return false
	}
	for i, target := range a.Targets {
		if target.Blocked != b.Targets[i].Blocked || target.BanKind != b.Targets[i].BanKind {
			return false
		}
	}
	return true
}

func uncertainInitialCard(c SharedCase) bool {
	return c.NotificationID == 0 && (c.SendState == "unknown" || c.SendState == "armed")
}

func sharedTerminal(c SharedCase) bool {
	if !c.Decided {
		return c.NeedsReview && len(c.PriorIntents) > 20
	}
	for _, choice := range c.Choices {
		found := false
		for _, r := range c.Reports {
			if r.Peer != choice.Report.Peer {
				continue
			}
			found = r.State == "accepted"
			for _, a := range r.Actions {
				if a.State == Pending || a.State == Running || a.Uncertain {
					return false
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Submit deduplicates bot-scoped updates and joins repeated searches only by a
// frozen set of exact originals and numeric provenance, never text or names.
func (s *SharedStore) Submit(ctx context.Context, r SharedReport, now time.Time) error {
	if r.Policy != "" && r.Policy != ForwardAllPolicy {
		return errors.New("unsupported shared instruction policy")
	}
	if !slices.Contains(s.peers, r.Peer) || r.Operator <= 0 || r.ChatID == 0 || r.Delivery.UpdateID <= 0 ||
		r.Delivery.ChatID == 0 || r.Delivery.MessageID <= 0 || len(r.Targets) > 20 || len(r.Actions) > 100 ||
		len(r.Refusals) > 40 ||
		len(r.Name) > 320 || len(r.SearchError) > 2000 {
		return errors.New("invalid shared report")
	}
	r.Targets = slices.Clone(r.Targets)
	for i := range r.Targets {
		r.Targets[i].Text = ""
	}
	j := s.journal
	return j.transaction(ctx, func(tx journalTx) error {
		all, err := s.cases(ctx, tx)
		if err != nil {
			return err
		}
		if e := s.decorateCases(ctx, tx, all); e != nil {
			return e
		}
		rawReport, marshalErr := json.Marshal(r)
		if marshalErr != nil {
			return journalError(marshalErr)
		}
		if e := j.exec(ctx, tx, `INSERT INTO shared_searches VALUES (?,?,?,?,?) ON CONFLICT(gid,peer,destination,message_id) DO UPDATE SET payload=excluded.payload`, j.db.GID(), r.Peer, r.Delivery.ChatID, r.Delivery.MessageID, string(rawReport)); e != nil {
			return e
		}
		var deliveries []struct {
			UpdateID    int    `db:"update_id"`
			Destination int64  `db:"destination"`
			MessageID   int    `db:"message_id"`
			Operator    int64  `db:"operator_id"`
			Ref         string `db:"ref"`
		}
		err = tx.SelectContext(ctx, &deliveries, j.db.Adopt(`SELECT update_id,destination,message_id,operator_id,ref FROM shared_deliveries WHERE gid=? AND peer=? AND (update_id=? OR (destination=? AND message_id=?))`), j.db.GID(), r.Peer, r.Delivery.UpdateID, r.Delivery.ChatID, r.Delivery.MessageID)
		if err != nil {
			return journalError(err)
		}
		ref := ""
		if len(deliveries) == 0 {
			var total int
			if e := tx.GetContext(ctx, &total, j.db.Adopt(`SELECT count(*) FROM shared_deliveries WHERE gid=?`), j.db.GID()); e != nil {
				return journalError(e)
			}
			if total >= 2500 {
				return ErrCapacity
			}
		}
		if len(deliveries) > 0 {
			d := deliveries[0]
			if len(deliveries) != 1 || d.UpdateID != r.Delivery.UpdateID || d.Destination != r.Delivery.ChatID ||
				d.MessageID != r.Delivery.MessageID || d.Operator != r.Operator {
				return errors.New("conflicting shared delivery")
			}
			ref = d.Ref
		} else {
			// Another bot may already have received this same admin message.
			var refs []string
			if err = tx.SelectContext(ctx, &refs, j.db.Adopt(`SELECT DISTINCT ref FROM shared_deliveries WHERE gid=? AND destination=? AND message_id=?`), j.db.GID(), r.Delivery.ChatID, r.Delivery.MessageID); err != nil {
				return journalError(err)
			}
			if len(refs) > 1 {
				return errors.New("conflicting shared case links")
			}
			if len(refs) == 1 {
				ref = refs[0]
			}
		}
		index := -1
		for i, c := range all {
			if c.Ref == ref {
				index = i
			}
		}
		// Coalesce a new unsent provisional case with an existing active inquiry.
		if len(deliveries) == 0 && (index < 0 || all[index].NotificationID == 0 && all[index].SendState == "" && !all[index].Decided) {
			for i, c := range all {
				if i == index || c.Canonical != "" || c.Policy != r.Policy ||
					(c.Decided && !uncertainInitialCard(c) && c.Policy != ForwardAllPolicy) || c.AdminID != r.Delivery.ChatID ||
					!uncertainInitialCard(c) && (c.Expires <= now.UnixMilli() || sharedTerminal(c)) {
					continue
				}
				matches := false
				for _, previous := range c.Reports {
					matches = matches || sameInstructionEpoch(previous, r)
				}
				if !matches {
					continue
				}
				compatible := true
				if index >= 0 {
					for _, x := range all[index].Reports {
						for _, y := range c.Reports {
							if x.Peer == y.Peer && (len(x.Targets) != 0 || len(y.Targets) != 0) && !sameInstructionEpoch(x, y) {
								compatible = false
							}
						}
					}
				}
				if !compatible {
					continue
				}
				if index >= 0 {
					old := all[index]
					for _, x := range old.Reports {
						exists := false
						for _, y := range c.Reports {
							exists = exists || x.Peer == y.Peer
						}
						if !exists {
							c.Reports = append(c.Reports, x)
							c.Revision++
						}
					}
					all[i] = c
					if e := j.exec(ctx, tx, `UPDATE shared_deliveries SET ref=? WHERE gid=? AND ref=?`, c.Ref, j.db.GID(), old.Ref); e != nil {
						return e
					}
					if e := j.exec(ctx, tx, `DELETE FROM shared_cases WHERE gid=? AND ref=?`, j.db.GID(), old.Ref); e != nil {
						return e
					}
				}
				index = i
				break
			}
		}
		var c SharedCase
		if index < 0 {
			if len(all) >= 500 {
				return ErrCapacity
			}
			c = SharedCase{Policy: r.Policy, Ref: token(), AdminID: r.Delivery.ChatID, Operator: r.Operator, Created: now.UnixMilli(),
				Expires: now.Add(15 * time.Minute).UnixMilli(), CollectUntil: now.Add(2 * time.Second).UnixMilli(), Revision: 1}
		} else {
			c = all[index]
		}
		found := false
		for i, old := range c.Reports {
			if old.Peer != r.Peer {
				continue
			}
			found = true
			if len(old.Targets) != len(r.Targets) || len(r.Targets) > 0 && !sameOriginals(old, r) {
				if old.Delivery.MessageID == r.Delivery.MessageID {
					return errors.New("shared targets changed within delivery")
				}
				// A new original set gets a fresh inquiry; an earlier selection never expands.
				if len(all) >= 500 {
					return ErrCapacity
				}
				c = SharedCase{Policy: r.Policy, Ref: token(), AdminID: r.Delivery.ChatID, Operator: r.Operator, Created: now.UnixMilli(),
					Expires: now.Add(15 * time.Minute).UnixMilli(), CollectUntil: now.Add(2 * time.Second).UnixMilli(), Revision: 1}
				var searches []string
				if e := tx.SelectContext(ctx, &searches, j.db.Adopt(`SELECT payload FROM shared_searches WHERE gid=? AND destination=? AND message_id=? ORDER BY peer`), j.db.GID(), r.Delivery.ChatID, r.Delivery.MessageID); e != nil {
					return journalError(e)
				}
				for _, raw := range searches {
					var report SharedReport
					if e := json.Unmarshal([]byte(raw), &report); e != nil {
						return journalError(e)
					}
					c.Reports = append(c.Reports, report)
				}
				if e := j.exec(ctx, tx, `UPDATE shared_deliveries SET ref=? WHERE gid=? AND destination=? AND message_id=?`, c.Ref, j.db.GID(), r.Delivery.ChatID, r.Delivery.MessageID); e != nil {
					return e
				}
				found = true
				break
			}
			// Repeated forwards retain the original receipt and its decision epoch.
			if old.Receipt == r.Receipt {
				if !slices.Equal(old.Refusals, r.Refusals) {
					c.Revision++
				}
				c.Reports[i] = r
			}
		}
		if !found {
			c.Reports = append(c.Reports, r)
			c.Revision++
		}
		if r.Expires > 0 {
			c.Expires = min(c.Expires, r.Expires)
		}
		slices.SortFunc(c.Reports, func(a, b SharedReport) int {
			if a.Peer < b.Peer {
				return -1
			}
			if a.Peer > b.Peer {
				return 1
			}
			return 0
		})
		if len(deliveries) == 0 {
			if e := j.exec(ctx, tx, `INSERT INTO shared_deliveries VALUES (?,?,?,?,?,?,?)`, j.db.GID(), r.Peer, r.Delivery.UpdateID, r.Delivery.ChatID, r.Delivery.MessageID, r.Operator, c.Ref); e != nil {
				return e
			}
		}
		return s.save(ctx, tx, c)
	})
}

// Cases returns a bounded consistent snapshot. No network effects occur here.
func (s *SharedStore) Cases(ctx context.Context) (out []SharedCase, err error) {
	err = s.journal.readCardTransaction(ctx, func(tx journalTx) error {
		out, err = s.cases(ctx, tx)
		if err != nil {
			return err
		}
		return s.decorateCases(ctx, tx, out)
	})
	return out, err
}

// Decide binds the owner bot, current revision, original requester and actual
// Telegram anchor. A choice cannot acquire later reports or new originals.
func (s *SharedStore) Decide(ctx context.Context, peer int64, ref string, revision int,
	admin int64, anchor int, operator, targetPeer int64, targetIndex int, now time.Time) error {
	return s.change(ctx, ref, func(tx journalTx, c *SharedCase) error {
		if c.Policy == ForwardAllPolicy {
			return errors.New("forward instruction cards do not authorize callback decisions")
		}
		if !s.anchorBound(ctx, tx, *c, peer, admin, anchor) || c.Canonical != "" || c.Revision != revision || c.AdminID != admin ||
			c.NotificationID != anchor || anchor <= 0 ||
			operator <= 0 || c.Expires <= now.UnixMilli() || c.Decided {
			return errors.New("stale or unbound shared callback")
		}
		if len(c.PriorIntents) > 20 {
			return ErrCapacity
		}
		c.DecisionOperator = operator
		c.DecidedAt = now.UnixMilli()
		c.Automatic = false
		c.NeedsReview = false
		return s.choose(c, targetPeer, targetIndex)
	})
}

func (s *SharedStore) choose(c *SharedCase, peer int64, index int) error {
	choices := []SharedChoice{}
	for _, r := range c.Reports {
		if peer != 0 && peer != r.Peer {
			continue
		}
		selected := []int{}
		for i, t := range r.Targets {
			if t.Blocked == "" && (index < 0 || i == index) {
				selected = append(selected, i)
			}
		}
		if len(selected) > 0 {
			choices = append(choices, SharedChoice{Report: r, Selected: selected})
		}
	}
	if len(choices) == 0 {
		return errors.New("no admissible listed originals")
	}
	c.Choices = choices
	c.Decided = true
	c.Revision++
	return nil
}

func (s *SharedStore) change(ctx context.Context, ref string, fn func(journalTx, *SharedCase) error) error {
	return s.journal.transaction(ctx, func(tx journalTx) error {
		c, err := s.readCase(ctx, tx, ref)
		if err != nil {
			return err
		}
		if applyErr := fn(tx, &c); applyErr != nil {
			return applyErr
		}
		return s.save(ctx, tx, c)
	})
}

func (s *SharedStore) readCase(ctx context.Context, tx journalTx, ref string) (SharedCase, error) {
	var c SharedCase
	var raw string
	j := s.journal
	if err := tx.GetContext(ctx, &raw, j.db.Adopt(`SELECT payload FROM shared_cases WHERE gid=? AND ref=?`), j.db.GID(), ref); err != nil {
		return c, journalError(err)
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, journalError(err)
	}
	return c, nil
}

// Case returns one consistent saved inquiry without scanning unrelated history.
func (s *SharedStore) Case(ctx context.Context, ref string) (out SharedCase, err error) {
	err = s.journal.readCardTransaction(ctx, func(tx journalTx) error {
		out, err = s.readCase(ctx, tx, ref)
		if err != nil {
			return err
		}
		a, e := s.readAnchor(ctx, tx, out.Ref)
		if e != nil {
			return e
		}
		applyAnchor(&out, a)
		return nil
	})
	return out, err
}

// AutoChoose preserves the existing single-original flow only after every peer
// responded and only if that original does not require an explicit new attempt.
func (s *SharedStore) AutoChoose(ctx context.Context, peer int64, ref string, now time.Time) error {
	return s.change(ctx, ref, func(_ journalTx, c *SharedCase) error {
		if c.Policy == ForwardAllPolicy {
			return s.continueForward(c, peer, now)
		}
		if peer != s.owner || c.Canonical != "" || c.Decided || c.NeedsReview ||
			c.Expires <= now.UnixMilli() || len(c.Reports) != len(s.peers) {
			return nil
		}
		count := 0
		var only SharedReport
		for _, r := range c.Reports {
			if r.SearchError != "" {
				return nil
			}
			count += len(r.Targets)
			if len(r.Targets) > 0 {
				only = r
			}
		}
		if count != 1 || only.Targets[0].Blocked != "" || len(only.Refusals) > 0 ||
			only.Reason != "initial" && only.Reason != "ongoing" && only.Reason != "already_confirmed" {
			return nil
		}
		c.DecisionOperator = c.Operator
		c.DecidedAt = now.UnixMilli()
		c.Automatic = true
		return s.choose(c, only.Peer, 0)
	})
}

// continueForward appends at most one frozen decision per participant. It never
// mutates a saved choice, retries a refusal or extends the original instruction.
func (s *SharedStore) continueForward(c *SharedCase, peer int64, now time.Time) error {
	if peer != s.owner || !c.HasPendingInstruction(now) {
		return nil
	}
	for _, r := range c.Reports {
		if !c.pendingInstructionReport(r, now) {
			continue
		}
		selected := []int{}
		for i, target := range r.Targets {
			if target.Blocked == "" {
				selected = append(selected, i)
			}
		}
		if len(selected) == 0 {
			continue
		}
		r.Expires = min(r.Expires, c.Expires)
		c.Choices = append(c.Choices, SharedChoice{Report: r, Selected: selected, DecidedAt: now.UnixMilli()})
		c.DecisionOperator = c.Operator
		if c.DecidedAt == 0 {
			c.DecidedAt = now.UnixMilli()
		}
		c.Automatic, c.Decided = true, true
		c.Revision++
	}
	return nil
}

// HasPendingInstruction is a read-only scheduling hint. AutoChoose repeats this
// check inside the SQL transaction; it is not action authority. Completed history
// must not cause periodic Telegram role lookups.
func (c SharedCase) HasPendingInstruction(now time.Time) bool {
	if c.Policy != ForwardAllPolicy || c.Canonical != "" || c.Expires <= now.UnixMilli() ||
		c.CollectUntil > now.UnixMilli() {
		return false
	}
	return slices.ContainsFunc(c.Reports, func(r SharedReport) bool { return c.pendingInstructionReport(r, now) })
}

func (c SharedCase) pendingInstructionReport(r SharedReport, now time.Time) bool {
	return r.Policy == ForwardAllPolicy && r.Operator == c.Operator && r.Receipt != "" &&
		r.SearchError == "" && r.State == "pending" && r.Expires > now.UnixMilli() &&
		!slices.ContainsFunc(c.Choices, func(ch SharedChoice) bool { return ch.Report.Peer == r.Peer }) &&
		slices.ContainsFunc(r.Targets, func(t ForwardTarget) bool { return t.Blocked == "" })
}

// RequireReview invalidates delayed dispatch authority if local facts changed.
func (s *SharedStore) RequireReview(ctx context.Context, ref string, peer int64, reason string) error {
	if len(reason) > 512 {
		return errors.New("shared review reason exceeds bound")
	}
	return s.change(ctx, ref, func(_ journalTx, c *SharedCase) error {
		if c.Policy == ForwardAllPolicy {
			// Keep the failed choice as a fence. Only another explicit intake can
			// authorize a new attempt; other late participants may still continue.
			c.NeedsReview = true
			c.ReviewReason = reason
			for i := range c.Choices {
				if c.Choices[i].Report.Peer == peer {
					c.Choices[i].Refusal = reason
				}
			}
			return nil
		}
		if !c.Decided {
			return nil
		}
		allowed := false
		for _, choice := range c.Choices {
			allowed = allowed || choice.Report.Peer == peer
		}
		if !allowed {
			return errors.New("shared review peer has no saved choice")
		}
		intent := SharedIntent{Operator: c.DecisionOperator, DecidedAt: c.DecidedAt, Automatic: c.Automatic, Reason: reason}
		for _, choice := range c.Choices {
			entry := SharedIntentChoice{Peer: choice.Report.Peer, Receipt: choice.Report.Receipt}
			for _, index := range choice.Selected {
				entry.MessageIDs = append(entry.MessageIDs, choice.Report.Targets[index].MessageID)
			}
			intent.Choices = append(intent.Choices, entry)
		}
		c.PriorIntents = append(c.PriorIntents, intent)
		c.ReviewReason = reason
		c.Choices = nil
		c.Decided = false
		c.Automatic = false
		c.NeedsReview = true
		c.Revision++
		return nil
	})
}

func (s *SharedStore) completeCase(c SharedCase) bool {
	if len(c.Reports) != len(s.peers) {
		return false
	}
	for _, r := range c.Reports {
		if r.SearchError != "" {
			return false
		}
	}
	return true
}

func sameInquiry(a, b SharedCase) bool {
	if a.AdminID != b.AdminID || a.Policy != b.Policy || len(a.Reports) != len(b.Reports) {
		return false
	}
	found := false
	for i, r := range a.Reports {
		u := b.Reports[i]
		if r.Peer != u.Peer || r.ChatID != u.ChatID {
			return false
		}
		if a.Policy == ForwardAllPolicy && !slices.Equal(r.Refusals, u.Refusals) {
			return false // a new explicit intake observed a different refusal epoch
		}
		if len(r.Targets) == 0 && len(u.Targets) == 0 {
			continue
		}
		if !sameInstructionEpoch(r, u) {
			return false
		}
		found = true
	}
	return found
}

// Reconcile coalesces inquiry records after every peer proves the same exact
// original set. Only the canonical card retains action buttons; decisions stay separate.
func (s *SharedStore) Reconcile(ctx context.Context, peer int64, now time.Time) error {
	if peer != s.owner {
		return errors.New("only card owner reconciles inquiries")
	}
	j := s.journal
	return j.transaction(ctx, func(tx journalTx) error {
		cases, err := s.cases(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.decorateCases(ctx, tx, cases); err != nil {
			return err
		}
		for i := range cases {
			a := &cases[i]
			if a.Canonical != "" || a.Decided && a.Policy != ForwardAllPolicy || a.Expires <= now.UnixMilli() ||
				sharedTerminal(*a) && a.Policy != ForwardAllPolicy || !s.completeCase(*a) {
				continue
			}
			for k := i + 1; k < len(cases); k++ {
				b := &cases[k]
				if b.Canonical != "" || b.Decided && b.Policy != ForwardAllPolicy || b.Expires <= now.UnixMilli() ||
					sharedTerminal(*b) && b.Policy != ForwardAllPolicy ||
					!s.completeCase(*b) || !sameInquiry(*a, *b) {
					continue
				}
				canonical, alias := a, b
				if a.NotificationID == 0 && b.NotificationID > 0 {
					canonical, alias = b, a
				}
				alias.Canonical = canonical.Ref
				alias.CanonicalAnchor = canonical.NotificationID
				alias.Revision++
				if err := s.save(ctx, tx, *alias); err != nil {
					return err
				}
				if err := j.exec(ctx, tx, `UPDATE shared_deliveries SET ref=? WHERE gid=? AND ref=?`, canonical.Ref, j.db.GID(), alias.Ref); err != nil {
					return err
				}
				if alias == a {
					break
				}
			}
		}
		return nil
	})
}

// Navigate persists the details page while retaining callback binding.
func (s *SharedStore) Navigate(ctx context.Context, peer int64, ref string, revision int, admin int64, anchor, page int) error {
	return s.change(ctx, ref, func(tx journalTx, c *SharedCase) error {
		if !s.anchorBound(ctx, tx, *c, peer, admin, anchor) || c.Revision != revision ||
			c.AdminID != admin || c.NotificationID != anchor ||
			anchor <= 0 || page < 0 || page > 100 {
			return fmt.Errorf("invalid shared navigation")
		}
		c.Page = page
		return nil
	})
}
