package approved

import (
	"fmt"
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
	UserID    string    `json:"user_id"`
	UserName  string    `json:"user_name"`
	Timestamp time.Time `json:"timestamp"`
	Count     int       `json:"-"`
	Source    string    `json:"-"`
}

func (u *UserInfo) String() string {
	if u.UserName == "" {
		return fmt.Sprintf("%q", u.UserID)
	}
	return fmt.Sprintf("%q (%s)", u.UserName, u.UserID)
}
