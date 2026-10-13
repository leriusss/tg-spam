package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
)

func TestRecoveryHTTPStatusAndRetryAfter(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var paused time.Time
			c := &telegramHTTPClient{
				recoveryHTTP: true,
				pause:        func(_ context.Context, until time.Time) error { paused = until; return nil },
				client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: code,
						Header:     http.Header{"Retry-After": []string{"7200"}},
						Body:       io.NopCloser(strings.NewReader("<html>gateway failure</html>")),
					}, nil
				}),
			}
			req, err := http.NewRequest(
				"POST",
				"https://api.telegram.org/bot"+fakeTelegramToken+"/deleteMessage",
				http.NoBody,
			)
			require.NoError(t, err)
			_, err = c.Do(req)
			var apiErr *tbapi.Error
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, code, apiErr.Code)
			require.NotContains(t, err.Error(), fakeTelegramToken)
			require.Equal(t, 7200, apiErr.RetryAfter)
			if code == 429 {
				require.Greater(t, time.Until(paused), 7199*time.Second)
			}
		})
	}
}

func TestRecoveryHTTPRequestLifetimeIncludesBodyAndPollingCooldown(t *testing.T) {
	for _, method := range []string{"deleteMessage", "getUpdates"} {
		t.Run(method, func(t *testing.T) {
			var observed context.Context
			c := &telegramHTTPClient{
				recoveryHTTP: true,
				client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
					observed = req.Context()
					deadline, ok := observed.Deadline()
					require.True(t, ok)
					maxLifetime := 15 * time.Second
					if method == "getUpdates" {
						maxLifetime = 90 * time.Second
					}
					require.LessOrEqual(t, time.Until(deadline), maxLifetime)
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
					}, nil
				}),
			}
			req, _ := http.NewRequest("POST", "https://api.telegram.org/bot"+fakeTelegramToken+"/"+method, http.NoBody)
			resp, err := c.Do(req)
			require.NoError(t, err)
			require.NoError(t, observed.Err(), "returning headers must not cancel body decoding")
			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.ErrorIs(t, observed.Err(), context.Canceled)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	c := &telegramHTTPClient{
		cooldown: func(context.Context) (time.Time, error) { return time.Now().Add(time.Hour), nil },
		client:   httpDoerFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, nil }),
	}
	req, _ := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://api.telegram.org/bot"+fakeTelegramToken+"/getUpdates",
		http.NoBody,
	)
	_, err := c.Do(req)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, called, "polling must also respect the persisted 429 barrier")
}
