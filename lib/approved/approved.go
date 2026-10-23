package approved

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	// SourceManual records explicit administrator approval.
	SourceManual = "manual"
	// SourceAuto records automatic approval.
	SourceAuto = "auto"
	// SourceUnknown records approval without known provenance.
	SourceUnknown = "unknown"
)

// UserInfo is a struct for approved user info.
type UserInfo struct {
	UserID    string                  `json:"user_id"`
	UserName  string                  `json:"user_name"`
	Timestamp time.Time               `json:"timestamp"`
	Count     int                     `json:"-"`
	Source    string                  `json:"-"`
	Progress  map[int64]CheckProgress `json:"-"`
}

// CheckProgress records completed checks and a bounded replay window in one source chat.
type CheckProgress struct {
	Count    int   `json:"count"`
	Messages []int `json:"messages,omitempty"`
}

// Clone copies mutable progress before publishing or returning a user snapshot.
func (u UserInfo) Clone() UserInfo {
	u.Progress = maps.Clone(u.Progress)
	for chat, progress := range u.Progress {
		progress.Messages = slices.Clone(progress.Messages)
		u.Progress[chat] = progress
	}
	return u
}

func (u *UserInfo) String() string {
	if u.UserName == "" {
		return fmt.Sprintf("%q", u.UserID)
	}
	return fmt.Sprintf("%q (%s)", u.UserName, u.UserID)
}
