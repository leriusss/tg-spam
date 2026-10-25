package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const startupUserJSON = `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture","username":"fixture_bot"}}`

type startupTemporaryTransport struct{}

func (startupTemporaryTransport) Error() string   { return "fixture temporary transport" }
func (startupTemporaryTransport) Timeout() bool   { return false }
func (startupTemporaryTransport) Temporary() bool { return true }

type startupTestClock struct {
	mu      sync.Mutex
	current time.Time
	delays  []time.Duration
	logs    []string
}

func newStartupTestClock() *startupTestClock { return &startupTestClock{current: time.Now()} }

func (c *startupTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *startupTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

func (c *startupTestClock) policy() telegramStartup {
	return telegramStartup{now: c.now, wait: func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.delays = append(c.delays, d)
		c.advance(d)
		return nil
	}, logf: func(format string, args ...any) { c.logs = append(c.logs, fmt.Sprintf(format, args...)) }}
}

type startupTestBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b startupTestBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func startupResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestTelegramStartupHTTP(t *testing.T) {
	for _, code := range []int{200, 400, 401, 403, 429, 500, 501, 502, 503, 504, 599} {
		for _, recovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/recovery=%t", code, recovery), func(t *testing.T) {
				var requests, closed atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/bot"+fakeTelegramToken+"/getMe", r.URL.Path)
					if requests.Add(1) == 1 && code != 200 {
						w.WriteHeader(code)
						if code == 429 {
							_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`)
						} else {
							_, _ = io.WriteString(w, "<html>"+fakeTelegramToken+"</html>")
						}
						return
					}
					_, _ = io.WriteString(w, startupUserJSON)
				}))
				defer server.Close()
				clock := newStartupTestClock()
				var contexts []context.Context
				client := &telegramHTTPClient{recoveryHTTP: recovery, client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
					deadline, ok := req.Context().Deadline()
					require.True(t, ok)
					require.InDelta(t, 15, time.Until(deadline).Seconds(), 0.5)
					contexts = append(contexts, req.Context())
					resp, err := server.Client().Do(req)
					if resp != nil {
						resp.Body = startupTestBody{ReadCloser: resp.Body, closed: &closed}
					}
					return resp, err
				})}
				api, err := clock.policy().initialize(context.Background(), fakeTelegramToken, server.URL+"/bot%s/%s", client)
				if code == 400 || code == 401 || code == 403 {
					require.Error(t, err)
					require.Nil(t, api)
					require.EqualValues(t, 1, requests.Load())
					require.Empty(t, clock.delays)
				} else {
					require.NoError(t, err)
					require.EqualValues(t, 777, api.Self.ID)
					require.Equal(t, 100, api.Buffer)
					require.Same(t, client, api.Client, "no canceled context or retry adapter may escape initialization")
					if code == 200 {
						require.EqualValues(t, 1, requests.Load())
						require.Empty(t, clock.delays)
					} else {
						require.EqualValues(t, 2, requests.Load())
						delay := time.Second
						if code == 429 {
							delay = 7 * time.Second
						}
						require.Equal(t, []time.Duration{delay}, clock.delays)
					}
				}
				require.Equal(t, requests.Load(), closed.Load(), "every response must close exactly once")
				for _, ctx := range contexts {
					require.Error(t, ctx.Err())
				}
				all := strings.Join(clock.logs, "\n") + fmt.Sprint(err)
				require.NotContains(t, all, fakeTelegramToken)
				require.NotContains(t, all, server.URL)
			})
		}
	}
}

func TestTelegramStartupTransportAndPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"timeout", context.DeadlineExceeded, true},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"other typed temporary transport", startupTemporaryTransport{}, true},
		{"temporary DNS", &net.DNSError{Err: "fixture", IsTemporary: true}, true},
		{"DNS timeout", &net.DNSError{Err: "fixture", IsTimeout: true}, true},
		{"DNS NXDOMAIN", &net.DNSError{Err: "fixture", IsNotFound: true}, false},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"permanent configuration", errors.New("invalid proxy " + fakeTelegramToken), false},
		{"untyped timeout text", errors.New("timeout connection reset " + fakeTelegramToken), false},
		{"canceled", context.Canceled, false},
		{"cooldown storage failure is not transport", cooldownUnavailable{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newStartupTestClock()
			calls := 0
			client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, &url.Error{Op: "Post", URL: "https://api.telegram.org/bot" + fakeTelegramToken + "/getMe", Err: tc.err}
				}
				return startupResponse(startupUserJSON), nil
			})}
			_, err := clock.policy().initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
			if tc.retry {
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, []time.Duration{time.Second}, clock.delays)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, calls)
				require.Empty(t, clock.delays)
			}
			require.NotContains(t, fmt.Sprint(err)+strings.Join(clock.logs, "\n"), fakeTelegramToken)
		})
	}
	for _, token := range []string{"", "bad", "123:", "-1:secret", "123:secret/escape", "123:secret?key", " 123:secret"} {
		clock := newStartupTestClock()
		calls := 0
		client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("must not request")
		})}
		_, err := clock.policy().initialize(context.Background(), token, tbapi.APIEndpoint, client)
		require.ErrorContains(t, err, "invalid_token")
		require.Zero(t, calls)
	}
}

func TestTelegramStartupAttemptsAndBudget(t *testing.T) {
	for _, requestTime := range []time.Duration{0, 15 * time.Second} {
		t.Run(requestTime.String(), func(t *testing.T) {
			clock := newStartupTestClock()
			start := clock.now()
			calls := 0
			client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				clock.advance(requestTime)
				return nil, context.DeadlineExceeded
			})}
			_, err := clock.policy().initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
			require.ErrorContains(t, err, "attempt=5/5")
			require.Equal(t, 5, calls)
			require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}, clock.delays)
			require.LessOrEqual(t, clock.now().Sub(start), 90*time.Second)
		})
	}
	t.Run("rate limit cannot reserve next attempt", func(t *testing.T) {
		clock := newStartupTestClock()
		calls := 0
		client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			clock.advance(15 * time.Second)
			return &http.Response{StatusCode: 429, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"ok":false,"parameters":{"retry_after":61}}`))}, nil
		})}
		_, err := clock.policy().initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
		require.ErrorContains(t, err, "budget_exhausted")
		require.Equal(t, 1, calls)
		require.Empty(t, clock.delays)
	})
	t.Run("elapsed budget blocks sixth or late attempt", func(t *testing.T) {
		clock := newStartupTestClock()
		policy := clock.policy()
		policy.wait = func(context.Context, time.Duration) error { clock.advance(90 * time.Second); return nil }
		calls := 0
		client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, syscall.ECONNRESET
		})}
		_, err := policy.initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
		require.ErrorContains(t, err, "budget_exhausted")
		require.Equal(t, 1, calls)
	})
	t.Run("earlier parent deadline also reserves next request", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		clock := newStartupTestClock()
		calls := 0
		client := &telegramHTTPClient{client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 429, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"ok":false,"parameters":{"retry_after":20}}`))}, nil
		})}
		_, err := clock.policy().initialize(ctx, fakeTelegramToken, tbapi.APIEndpoint, client)
		require.ErrorContains(t, err, "budget_exhausted")
		require.Equal(t, 1, calls)
		require.Empty(t, clock.delays)
	})
}

func TestTelegramStartupCancellation(t *testing.T) {
	for _, phase := range []string{"before", "request", "wait", "success raced with cancellation"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newStartupTestClock()
			policy := clock.policy()
			calls := 0
			client := &telegramHTTPClient{client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if phase == "request" {
					cancel()
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				if phase == "success raced with cancellation" {
					cancel()
					return startupResponse(startupUserJSON), nil
				}
				return nil, syscall.ECONNRESET
			})}
			if phase == "before" {
				cancel()
			}
			if phase == "wait" {
				policy.wait = func(waitCtx context.Context, delay time.Duration) error {
					cancel()
					return waitTelegramStartup(waitCtx, delay)
				}
			}
			_, err := policy.initialize(ctx, fakeTelegramToken, tbapi.APIEndpoint, client)
			require.ErrorContains(t, err, "canceled")
			require.ErrorIs(t, err, context.Canceled)
			require.LessOrEqual(t, calls, 1)
		})
	}
}

func TestTelegramStartupDoesNotRetryRuntimeMethods(t *testing.T) {
	clock := newStartupTestClock()
	var methods []string
	client := &telegramHTTPClient{recoveryHTTP: true, client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		methods = append(methods, method)
		if method == "getMe" {
			return startupResponse(startupUserJSON), nil
		}
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		want := 15 * time.Second
		if method == "getUpdates" {
			want = 90 * time.Second
		}
		require.InDelta(t, want.Seconds(), time.Until(deadline).Seconds(), 0.5)
		return nil, syscall.ECONNRESET
	})}
	api, err := clock.policy().initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
	require.NoError(t, err)
	for _, method := range []string{"banChatSenderChat", "deleteMessage", "sendMessage", "getUpdates"} {
		_, err = api.MakeRequest(method, nil)
		require.Error(t, err)
	}
	require.Equal(t, []string{"getMe", "banChatSenderChat", "deleteMessage", "sendMessage", "getUpdates"}, methods)
	require.Empty(t, clock.delays)
}

func TestTelegramStartupRateLimitsAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, body, header string
		status             int
		wantDelay          time.Duration
		permanent          bool
	}{
		{"JSON", `{"ok":false,"parameters":{"retry_after":7}}`, "", 429, 7 * time.Second, false},
		{"larger header", `{"ok":false,"parameters":{"retry_after":2}}`, "9", 429, 9 * time.Second, false},
		{"larger JSON", `{"ok":false,"parameters":{"retry_after":9}}`, "2", 429, 9 * time.Second, false},
		{"HTTP date", `{"ok":false}`, "date", 429, 11 * time.Second, false},
		{"typed Bot API response", `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`, "", 200, 7 * time.Second, false},
		{"two hours never shortened", `{"ok":false,"parameters":{"retry_after":7200}}`, "", 429, 7200 * time.Second, false},
		{"missing delay fails closed", `{"ok":false}`, "", 429, 0, true},
		{"unreadable delay fails closed", `{"parameters":`, "", 429, 0, true},
		{"oversized delay response", strings.Repeat(" ", 65537), "", 429, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newStartupTestClock()
			clock.current = clock.current.Truncate(time.Second)
			start := clock.now()
			calls := 0
			var paused time.Time
			client := &telegramHTTPClient{pause: func(ctx context.Context, until time.Time) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 2*time.Second)
				paused = until
				return nil
			}, client: httpDoerFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					return startupResponse(startupUserJSON), nil
				}
				header := tc.header
				if header == "date" {
					header = start.Add(tc.wantDelay).UTC().Format(http.TimeFormat)
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": []string{header}},
					Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			_, err := clock.policy().initialize(context.Background(), fakeTelegramToken, tbapi.APIEndpoint, client)
			switch {
			case tc.permanent:
				require.Error(t, err)
				require.Equal(t, 1, calls)
				require.Empty(t, clock.delays)
			case tc.wantDelay > 75*time.Second:
				require.ErrorContains(t, err, "budget_exhausted")
				require.Equal(t, 1, calls)
				require.Empty(t, clock.delays)
				require.Equal(t, start.Add(tc.wantDelay), paused, "durable cooldown must not cap the server delay")
			default:
				require.NoError(t, err)
				require.Equal(t, 2, calls)
				require.Equal(t, []time.Duration{tc.wantDelay}, clock.delays)
				if tc.status == 429 {
					require.Equal(t, start.Add(tc.wantDelay), paused)
				}
			}
		})
	}
}

func TestTelegramStartupRealTLSIsPermanent(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, startupUserJSON)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	clock := newStartupTestClock()
	calls := 0
	client := &telegramHTTPClient{client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return http.DefaultClient.Do(req) // Deliberately do not trust the local test certificate.
	})}
	_, err := clock.policy().initialize(context.Background(), fakeTelegramToken, server.URL+"/bot%s/%s", client)
	require.ErrorContains(t, err, "tls_certificate")
	require.Equal(t, 1, calls)
	require.Empty(t, clock.delays)
	require.NotContains(t, fmt.Sprint(err)+strings.Join(clock.logs, "\n"), fakeTelegramToken)
}

func TestTelegramStartupRealRequestCancellation(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, stopped := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if phase == "body" {
					_, _ = io.WriteString(w, `{"ok":true,"result":`)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer server.Close()
			done := make(chan error, 1)
			go func() {
				_, err := initializeTelegram(ctx, fakeTelegramToken, server.URL+"/bot%s/%s",
					&telegramHTTPClient{client: server.Client()})
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request not started")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorContains(t, err, "canceled")
			case <-time.After(5 * time.Second):
				t.Fatal("request/body did not cancel")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP connection/goroutine was not released")
			}
			require.EqualValues(t, 1, calls.Load())
		})
	}
}
