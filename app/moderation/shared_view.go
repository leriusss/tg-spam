package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type sharedResolution struct {
	PreviousState            string
	NotificationID, Revision int
	At                       int64
}

// sharedAnchor owns delivery state for one inquiry, never for an admin chat.
// Legacy shared_views rows are retained as audit data and read only for their ref.
type sharedAnchor struct {
	AdminID                      int64
	Ref                          string
	NotificationID               int
	SendState, Lease, LastRender string
	LeaseUntil, NextAt           int64
	Attempts                     int
	Resolution                   *sharedResolution
}

// SharedView presents one inquiry without any global selected or pinned view.
type SharedView struct {
	Case      SharedCase
	PeerNames map[int64]string // presentation only, learned from saved participant reports
}

// ForeignNamespaceBlocked prevents legacy handlers from adopting known shared cards.
func (s *SharedStore) ForeignNamespaceBlocked(ctx context.Context, admin int64, notification int) (blocked bool, err error) {
	err = s.journal.readCardTransaction(ctx, func(tx journalTx) error {
		cases, e := s.cases(ctx, tx)
		if e != nil {
			return e
		}
		for _, c := range cases {
			if c.AdminID != admin {
				continue
			}
			a, e := s.readAnchor(ctx, tx, c.Ref)
			if e != nil {
				return e
			}
			if notification > 0 && notification == a.NotificationID {
				blocked = true
				break
			}
		}
		return nil
	})
	return blocked, err
}

func (s *SharedStore) readAnchor(ctx context.Context, tx journalTx, ref string) (sharedAnchor, error) {
	var rows []string
	j := s.journal
	if err := tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT payload FROM shared_cards WHERE gid=? AND ref=?`), j.db.GID(), ref); err != nil {
		return sharedAnchor{}, journalError(err)
	}
	a := sharedAnchor{Ref: ref}
	if len(rows) == 0 {
		// The draft's old layout had a single panel. Preserve its one actual binding,
		// including uncertainty, without copying that state onto unrelated inquiries.
		if err := tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT payload FROM shared_views WHERE gid=?`), j.db.GID()); err != nil {
			return a, journalError(err)
		}
		for _, raw := range rows {
			var legacy sharedAnchor
			if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
				return a, journalError(err)
			}
			if legacy.Ref == ref {
				return legacy, nil
			}
		}
		return a, nil
	}
	if err := json.Unmarshal([]byte(rows[0]), &a); err != nil {
		return a, journalError(err)
	}
	return a, nil
}

func (s *SharedStore) saveAnchor(ctx context.Context, tx journalTx, a sharedAnchor) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return journalError(err)
	}
	return s.journal.exec(ctx, tx, `INSERT INTO shared_cards VALUES (?,?,?) ON CONFLICT(gid,ref) DO UPDATE SET payload=excluded.payload`, s.journal.db.GID(), a.Ref, string(raw))
}

func applyAnchor(c *SharedCase, a sharedAnchor) {
	c.NotificationID = a.NotificationID
	c.SendState = a.SendState
	c.Lease = a.Lease
	c.LeaseUntil = a.LeaseUntil
	c.NextAt = a.NextAt
	c.Attempts = a.Attempts
	c.LastRender = a.LastRender
}

func (s *SharedStore) decorateCases(ctx context.Context, tx journalTx, cases []SharedCase) error {
	for i := range cases {
		a, err := s.readAnchor(ctx, tx, cases[i].Ref)
		if err != nil {
			return err
		}
		applyAnchor(&cases[i], a)
	}
	return nil
}

// View presents this inquiry only. Reading history cannot change another card.
func (s *SharedStore) View(ctx context.Context, peer int64, ref string, _ time.Time) (out SharedView, found bool, err error) {
	if peer != s.owner {
		return out, false, errors.New("only card owner presents inquiries")
	}
	err = s.journal.readCardTransaction(ctx, func(tx journalTx) error {
		c, e := s.readCase(ctx, tx, ref)
		if e != nil {
			return e
		}
		a, e := s.readAnchor(ctx, tx, ref)
		if e != nil {
			return e
		}
		applyAnchor(&c, a)
		if c.Canonical != "" {
			canonical, e := s.readAnchor(ctx, tx, c.Canonical)
			if e != nil {
				return e
			}
			c.CanonicalAnchor = canonical.NotificationID
		}
		out.Case = c
		found = true
		return nil
	})
	return out, found, err
}

func (s *SharedStore) anchorBound(ctx context.Context, tx journalTx, c SharedCase, peer, admin int64, anchor int) bool {
	if peer != s.owner || admin != c.AdminID || anchor <= 0 {
		return false
	}
	a, err := s.readAnchor(ctx, tx, c.Ref)
	return err == nil && a.NotificationID == anchor && a.SendState == "known"
}

// ResolveUnknown binds an independently verified existing message; it never sends,
// resets an inquiry, or changes moderation authority or original deadlines.
func (s *SharedStore) ResolveUnknown(ctx context.Context, peer int64, ref string, revision int, admin int64, anchor int) error {
	return s.journal.transaction(ctx, func(tx journalTx) error {
		c, e := s.readCase(ctx, tx, ref)
		if e != nil {
			return e
		}
		a, e := s.readAnchor(ctx, tx, ref)
		if e != nil {
			return e
		}
		if peer != s.owner || c.AdminID != admin || c.Revision != revision ||
			c.Canonical != "" || anchor <= 0 || a.SendState != "unknown" {
			return errors.New("unbound unknown-send resolution")
		}
		all, e := s.cases(ctx, tx)
		if e != nil {
			return e
		}
		for _, other := range all {
			known, e := s.readAnchor(ctx, tx, other.Ref)
			if e != nil {
				return e
			}
			if other.AdminID == admin && other.Ref != ref && known.NotificationID == anchor {
				return errors.New("message belongs to another inquiry")
			}
		}
		a.Resolution = &sharedResolution{PreviousState: a.SendState, NotificationID: anchor,
			Revision: revision, At: time.Now().UnixMilli()}
		a.AdminID = admin
		a.NotificationID = anchor
		a.SendState = "known"
		a.LastRender = ""
		a.Lease = ""
		a.LeaseUntil = 0
		c.Revision++ // old/unverified buttons never gain the resolution's authority
		if e := s.save(ctx, tx, c); e != nil {
			return e
		}
		return s.saveAnchor(ctx, tx, a)
	})
}

// ClaimCard fences sends/edits for this inquiry across processes and restarts.
func (s *SharedStore) ClaimCard(ctx context.Context, peer int64, ref string, revision int, rendered string,
	now time.Time) (out SharedCase, claimed bool, err error) {
	err = s.journal.transaction(ctx, func(tx journalTx) error {
		out, claimed = SharedCase{}, false // a rolled-back attempt grants no lease on retry
		c, e := s.readCase(ctx, tx, ref)
		if e != nil {
			return e
		}
		a, e := s.readAnchor(ctx, tx, c.Ref)
		if e != nil {
			return e
		}
		a.AdminID = c.AdminID
		if peer != s.owner || c.Revision != revision || c.Canonical != "" && a.NotificationID == 0 ||
			c.CollectUntil > now.UnixMilli() || a.NextAt > now.UnixMilli() ||
			a.LastRender == rendered || a.SendState == "unknown" || a.SendState == "failed" {
			return nil
		}
		if a.NotificationID == 0 && !s.completeCase(c) && now.UnixMilli() < c.CollectUntil+15000 {
			all, e := s.cases(ctx, tx)
			if e != nil {
				return e
			}
			for _, older := range all {
				if older.Ref == c.Ref {
					break
				}
				if older.AdminID != c.AdminID || older.Canonical != "" || older.Decided || older.Expires <= now.UnixMilli() {
					continue
				}
				distinct := false
				for _, x := range older.Reports {
					for _, y := range c.Reports {
						if x.Peer == y.Peer && (len(x.Targets) != 0 || len(y.Targets) != 0) && !sameOriginals(x, y) {
							distinct = true
						}
					}
				}
				if !distinct {
					return nil
				} // bounded collection, never a shared decision
			}
		}
		if a.Lease != "" {
			if a.LeaseUntil > now.UnixMilli() {
				return nil
			}
			if a.NotificationID == 0 {
				a.SendState = "unknown"
				a.Lease = ""
				return s.saveAnchor(ctx, tx, a)
			}
		}
		if a.NotificationID == 0 && a.SendState == "armed" {
			a.SendState = "unknown"
			return s.saveAnchor(ctx, tx, a)
		}
		a.Lease = token()
		a.LeaseUntil = now.Add(30 * time.Second).UnixMilli()
		a.SendState = "armed"
		a.Attempts++
		applyAnchor(&c, a)
		if e := s.save(ctx, tx, c); e != nil {
			return e
		}
		if e := s.saveAnchor(ctx, tx, a); e != nil {
			return e
		}
		out = c
		claimed = true
		return nil
	})
	if err != nil {
		return SharedCase{}, false, err
	}
	return out, claimed, err
}

// FinishCard acknowledges only the claimed inquiry and its lease.
func (s *SharedStore) FinishCard(ctx context.Context, c SharedCase, anchor int, rendered, outcome string, now time.Time) error {
	return s.journal.transaction(ctx, func(tx journalTx) error {
		a, e := s.readAnchor(ctx, tx, c.Ref)
		if e != nil {
			return e
		}
		if a.Lease == "" || a.Lease != c.Lease {
			return ErrLease
		}
		a.Lease = ""
		a.LeaseUntil = 0
		switch outcome {
		case "ok":
			a.NotificationID = anchor
			a.SendState = "known"
			a.LastRender = rendered
			a.Attempts = 0
		case "retry":
			a.SendState = ""
			a.NextAt = now.Add(10 * time.Second).UnixMilli()
			if a.Attempts >= 5 {
				a.SendState = "failed"
			}
		default:
			a.SendState = outcome
		}
		current, e := s.readCase(ctx, tx, c.Ref)
		if e != nil {
			return e
		}
		applyAnchor(&current, a)
		if e := s.save(ctx, tx, current); e != nil {
			return e
		}
		return s.saveAnchor(ctx, tx, a)
	})
}
