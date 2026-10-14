package events

import (
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // keep the named timezone available on Windows and minimal images

	"github.com/umputun/tg-spam/app/moderation"
)

func cardLocation() *time.Location {
	location, err := time.LoadLocation("Europe/Kaliningrad")
	if err != nil {
		return time.FixedZone("Europe/Kaliningrad", 2*60*60)
	}
	return location
}

func cardDate(seconds int64) string {
	return time.Unix(seconds, 0).In(cardLocation()).Format("02.01.2006 15:04:05") + " (Калининград)"
}

// currentCardActions projects recorded results without modifying historical operations.
func currentCardActions(c moderation.Card) []moderation.Action {
	groups := map[string][]moderation.Action{}
	keys := []string{}
	for _, op := range c.Actions {
		if op.Kind == "card" || op.Kind == "notice" {
			continue
		}
		key := op.Kind
		switch op.Kind {
		case "ban", "restrict", "unban", "restore":
			key = "membership"
		case "original_delete", "cleanup_delete":
			key = fmt.Sprintf("delete:%d:%d", op.ChatID, op.MessageID)
		}
		if _, exists := groups[key]; !exists {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], op)
	}
	result := []moderation.Action{}
	for _, key := range keys {
		ops := groups[key]
		nonCanceled := []moderation.Action{}
		for _, op := range ops {
			if op.State != moderation.Canceled {
				nonCanceled = append(nonCanceled, op)
			}
		}
		if len(nonCanceled) > 0 {
			ops = nonCanceled
		}
		slices.SortStableFunc(ops, func(a, b moderation.Action) int {
			if a.NextAt < b.NextAt {
				return -1
			}
			if a.NextAt > b.NextAt {
				return 1
			}
			return 0
		})
		last := ops[len(ops)-1]
		if key == "membership" && last.State != moderation.Confirmed {
			for i := len(ops) - 2; i >= 0; i-- {
				if ops[i].State == moderation.Confirmed {
					result = append(result, ops[i])
					break
				}
			}
		}
		// a confirmed deletion remains a fact even after a later absent replay.
		if strings.HasPrefix(key, "delete:") {
			for _, op := range ops {
				if op.State == moderation.Confirmed {
					last = op
				}
			}
			if last.MessageID == c.MessageID {
				last.Kind = "original_delete"
			}
		}
		result = append(result, last)
	}
	return result
}

func manualCardAction(op moderation.Action) bool {
	return op.State == moderation.Failed || op.State == moderation.Expired || op.State == moderation.Manual ||
		op.State == moderation.Blocked || op.Uncertain && op.State != moderation.Pending && op.State != moderation.Running
}

func cardWork(c moderation.Card) (live, manual, decision, undoBlocked bool) {
	for _, op := range c.Actions {
		if op.Kind == "card" || op.Kind == "notice" {
			continue
		}
		live = live || op.State == moderation.Pending || op.State == moderation.Running
		unclosed := manualCardAction(op) && !slices.Contains(c.Resolved, op.ID)
		manual = manual || unclosed
		undoBlocked = undoBlocked || (op.Kind == "unban" || op.Kind == "restore") &&
			op.State != moderation.Confirmed && op.State != moderation.Canceled && !slices.Contains(c.Resolved, op.ID)
	}
	manual = manual || c.IncompleteDecisions > 0
	for _, op := range currentCardActions(c) {
		if (op.Kind == "ban" || op.Kind == "restrict" || op.Kind == "original_delete") &&
			manualCardAction(op) && !slices.Contains(c.Resolved, op.ID) &&
			(op.Kind != "original_delete" || op.State != moderation.Expired || op.MessageDate <= 0 ||
				time.Now().Unix() < op.MessageDate+48*60*60) {
			decision = true
		}
	}
	decision = decision && !live && !undoBlocked && c.IncompleteDecisions == 0
	return live, manual, decision, undoBlocked
}
