package tgspam

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"github.com/umputun/tg-spam/lib/tgspam/mocks"
)

func TestDetector_CASDNSRecovery(t *testing.T) {
	down, calls := true, 0
	client := &mocks.HTTPClientMock{DoFunc: func(*http.Request) (*http.Response, error) {
		calls++
		if down {
			return nil, &net.DNSError{Name: "cas.invalid", Err: "server misbehaving", IsTemporary: true}
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"ok":true,"description":"spam detected"}`)),
		}, nil
	}}
	d := NewDetector(Config{
		CasAPI: "https://cas.invalid", HTTPClient: client, MaxAllowedEmoji: -1,
		FirstMessageOnly: true, FirstMessagesCount: 3,
	})
	spam, checks := d.Check(spamcheck.Request{UserID: "42", Msg: "обычное сообщение"})
	require.False(t, spam, "DNS unavailability is not evidence for banning")
	require.Equal(t, 3, calls)
	require.Contains(t, checks[0].Details, "server misbehaving")
	down = false
	spam, checks = d.Check(spamcheck.Request{UserID: "42", Msg: "следующее сообщение"})
	require.True(t, spam, "the next eligible message queries CAS again after recovery")
	require.Equal(t, 4, calls)
	require.True(t, checks[0].Spam)
}
