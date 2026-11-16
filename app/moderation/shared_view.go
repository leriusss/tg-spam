package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// sharedAnchor is the sole persistent Telegram message for an admin destination.
// Inquiry identity and immutable decisions remain in shared_cases, independently.
type sharedAnchor struct {
	Pinned                       bool
	AdminID                      int64
	Ref                          string
	NotificationID               int
	SendState, Lease, LastRender string
	LeaseUntil, NextAt           int64
	Attempts, Page               int
	Menu                         []string
}

// SharedMenuItem navigates between distinct inquiries without merging targets.
type SharedMenuItem struct {
	Ref       string
	MessageID int
	Decided   bool
}

// SharedView presents one inquiry and bounded navigation in one stable message.
type SharedView struct {
	Case SharedCase
	Menu []SharedMenuItem
	Page int
}

func (s *SharedStore) readAnchor(ctx context.Context, tx journalTx, admin int64) (sharedAnchor, error) {
	var rows []string
	j := s.journal
	if err := tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT payload FROM shared_views WHERE gid=? AND destination=?`), j.db.GID(), admin); err != nil {
		return sharedAnchor{}, journalError(err)
	}
	a := sharedAnchor{AdminID: admin}
	if len(rows) > 0 {
		if err := json.Unmarshal([]byte(rows[0]), &a); err != nil {
			return a, journalError(err)
		}
	}
	return a, nil
}

func (s *SharedStore) saveAnchor(ctx context.Context, tx journalTx, a sharedAnchor) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return journalError(err)
	}
	return s.journal.exec(ctx, tx, `INSERT INTO shared_views VALUES (?,?,?) ON CONFLICT(gid,destination) DO UPDATE SET payload=excluded.payload`, s.journal.db.GID(), a.AdminID, string(raw))
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
	anchors := map[int64]sharedAnchor{}
	for i := range cases {
		admin := cases[i].AdminID
		a, ok := anchors[admin]
		if !ok {
			var err error
			a, err = s.readAnchor(ctx, tx, admin)
			if err != nil {
				return err
			}
			anchors[admin] = a
		}
		applyAnchor(&cases[i], a)
	}
	return nil
}

// View keeps unrelated, late or unresolved inquiries behind the same anchor.
func (s *SharedStore) View(ctx context.Context, peer, admin int64, now time.Time) (out SharedView, found bool, err error) {
	if peer != s.owner {
		return out, false, errors.New("only card owner presents inquiries")
	}
	err = s.journal.transaction(ctx, func(tx journalTx) error {
		all, e := s.cases(ctx, tx)
		if e != nil {
			return e
		}
		a, e := s.readAnchor(ctx, tx, admin)
		if e != nil {
			return e
		}
		cases := []SharedCase{}
		for _, c := range all {
			if c.AdminID == admin && c.Canonical == "" {
				cases = append(cases, c)
			}
		}
		if len(cases) == 0 {
			return nil
		}
		index := -1
		for i, c := range cases {
			if c.Ref == a.Ref {
				index = i
			}
		}
		if index < 0 {
			index = 0
			a.Ref = cases[0].Ref
			a.Pinned = false
		}
		// Show the next pending inquiry automatically only after this one finishes.
		if !a.Pinned && (cases[index].Expires <= now.UnixMilli() || sharedTerminal(cases[index])) {
			for i, c := range cases {
				if c.Expires > now.UnixMilli() && !c.Decided {
					index = i
					a.Ref = c.Ref
					break
				}
			}
		}
		a.Menu = nil
		out.Menu = nil
		for _, c := range cases {
			mid := 0
			if len(c.Reports) > 0 {
				mid = c.Reports[0].Delivery.MessageID
			}
			a.Menu = append(a.Menu, c.Ref)
			out.Menu = append(out.Menu, SharedMenuItem{Ref: c.Ref, MessageID: mid, Decided: c.Decided})
		}
		a.Page = min(a.Page, (len(cases)-1)/5)
		out.Case = cases[index]
		applyAnchor(&out.Case, a)
		out.Page = a.Page
		found = true
		return s.saveAnchor(ctx, tx, a)
	})
	return out, found, err
}

func (s *SharedStore) anchorBound(ctx context.Context, tx journalTx, c SharedCase, peer, admin int64, anchor int) bool {
	if peer != s.owner || admin != c.AdminID || anchor <= 0 {
		return false
	}
	a, err := s.readAnchor(ctx, tx, admin)
	return err == nil && a.NotificationID == anchor && a.Ref == c.Ref
}

// SwitchView changes presentation only; it never changes a decision or expiry.
func (s *SharedStore) SwitchView(ctx context.Context, peer int64, ref string, revision int,
	admin int64, anchor, index, page int) error {
	return s.journal.transaction(ctx, func(tx journalTx) error {
		c, e := s.readCase(ctx, tx, ref)
		if e != nil {
			return e
		}
		if c.Revision != revision || !s.anchorBound(ctx, tx, c, peer, admin, anchor) {
			return errors.New("stale shared view")
		}
		a, e := s.readAnchor(ctx, tx, admin)
		if e != nil {
			return e
		}
		if index >= 0 {
			if index >= len(a.Menu) {
				return errors.New("shared inquiry outside displayed menu")
			}
			target, e := s.readCase(ctx, tx, a.Menu[index])
			if e != nil {
				return e
			}
			if target.AdminID != admin || target.Canonical != "" {
				return errors.New("shared inquiry superseded")
			}
			a.Ref = target.Ref
			a.Pinned = true
		} else {
			if page < 0 || page*5 >= len(a.Menu) {
				return errors.New("shared menu page outside range")
			}
			a.Page = page
		}
		return s.saveAnchor(ctx, tx, a)
	})
}

// ClaimCard fences sends/edits across every inquiry, including disjoint late searches.
func (s *SharedStore) ClaimCard(ctx context.Context, peer int64, ref, rendered string,
	now time.Time) (out SharedCase, claimed bool, err error) {
	err = s.journal.transaction(ctx, func(tx journalTx) error {
		c, e := s.readCase(ctx, tx, ref)
		if e != nil {
			return e
		}
		a, e := s.readAnchor(ctx, tx, c.AdminID)
		if e != nil {
			return e
		}
		if a.Ref == "" {
			a.Ref = ref
		}
		if peer != s.owner || a.Ref != ref || c.CollectUntil > now.UnixMilli() || a.NextAt > now.UnixMilli() ||
			a.LastRender == rendered || a.SendState == "unknown" || a.SendState == "failed" {
			return nil
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
	return out, claimed, err
}

// FinishCard acknowledges the same persistent anchor even after changing inquiries.
func (s *SharedStore) FinishCard(ctx context.Context, c SharedCase, anchor int, rendered, outcome string, now time.Time) error {
	return s.journal.transaction(ctx, func(tx journalTx) error {
		a, e := s.readAnchor(ctx, tx, c.AdminID)
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
