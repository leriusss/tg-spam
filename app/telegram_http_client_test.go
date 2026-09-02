package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeTelegramToken = "123456789:TEST_FAKE_TOKEN_DO_NOT_USE"

type httpDoerFunc func(req *http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type timeoutTestError struct {
	cause error
}

func (e timeoutTestError) Error() string   { return "request timed out: " + e.cause.Error() }
func (e timeoutTestError) Unwrap() error   { return e.cause }
func (e timeoutTestError) Timeout() bool   { return true }
func (e timeoutTestError) Temporary() bool { return true }

func TestSanitizeTelegramHTTPError(t *testing.T) {
	t.Run("getUpdates DNS error", func(t *testing.T) {
		underlying := &net.DNSError{Err: "no such host", Name: "api.telegram.org", IsNotFound: true}
		original := &url.Error{
			Op:  "Post",
			URL: "https://api.telegram.org/bot" + fakeTelegramToken + "/getUpdates",
			Err: underlying,
		}

		sanitized := sanitizeTelegramHTTPError(original)

		assert.NotContains(t, sanitized.Error(), fakeTelegramToken)
		assert.Contains(t, sanitized.Error(), "/botREDACTED/getUpdates")
		assert.True(t, errors.Is(sanitized, underlying))

		var gotURL *url.Error
		require.True(t, errors.As(sanitized, &gotURL))
		assert.Equal(t, original.Op, gotURL.Op)
		assert.Same(t, underlying, gotURL.Err)
	})

	t.Run("timeout semantics", func(t *testing.T) {
		cause := errors.New("deadline")
		original := &url.Error{
			Op:  "Post",
			URL: "https://api.telegram.org/bot" + fakeTelegramToken + "/getUpdates",
			Err: timeoutTestError{cause: cause},
		}

		sanitized := sanitizeTelegramHTTPError(original)

		assert.NotContains(t, sanitized.Error(), fakeTelegramToken)
		assert.True(t, errors.Is(sanitized, cause))
		var gotURL *url.Error
		require.True(t, errors.As(sanitized, &gotURL))
		assert.True(t, gotURL.Timeout())
		assert.True(t, gotURL.Temporary())
	})

	t.Run("wrapped underlying error", func(t *testing.T) {
		cause := errors.New("connection refused")
		original := &url.Error{
			Op:  "Post",
			URL: "https://api.telegram.org/bot" + fakeTelegramToken + "/deleteMessage",
			Err: fmt.Errorf("dial failed: %w", cause),
		}

		sanitized := sanitizeTelegramHTTPError(original)

		assert.True(t, errors.Is(sanitized, cause))
		var gotURL *url.Error
		assert.True(t, errors.As(sanitized, &gotURL))
	})

	for name, rawURL := range map[string]string{
		"normal Bot API method": "https://api.telegram.org/bot" + fakeTelegramToken + "/sendMessage",
		"file API":              "https://api.telegram.org/file/bot" + fakeTelegramToken + "/path/to/file.jpg",
	} {
		t.Run(name, func(t *testing.T) {
			original := &url.Error{Op: "Get", URL: rawURL, Err: errors.New("connection reset")}

			sanitized := sanitizeTelegramHTTPError(original)

			assert.NotContains(t, sanitized.Error(), fakeTelegramToken)
			assert.Contains(t, sanitized.Error(), "botREDACTED")
		})
	}

	t.Run("nil error", func(t *testing.T) {
		assert.NoError(t, sanitizeTelegramHTTPError(nil))
	})
}

func TestRedactTelegramBotURL(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		expected string
	}{
		{
			name:     "normal Bot API method",
			rawURL:   "https://api.telegram.org/bot" + fakeTelegramToken + "/sendMessage?trace=test",
			expected: "https://api.telegram.org/botREDACTED/sendMessage?trace=test",
		},
		{
			name:     "file API",
			rawURL:   "https://api.telegram.org/file/bot" + fakeTelegramToken + "/path/to/file.jpg",
			expected: "https://api.telegram.org/file/botREDACTED/path/to/file.jpg",
		},
		{
			name:     "escaped token segment",
			rawURL:   "https://api.telegram.org/bot123456789%3ATEST_FAKE_TOKEN_DO_NOT_USE/getUpdates",
			expected: "https://api.telegram.org/botREDACTED/getUpdates",
		},
		{
			name:     "custom Telegram endpoint",
			rawURL:   "https://telegram.internal/v1/bot" + fakeTelegramToken + "/sendMessage",
			expected: "https://telegram.internal/v1/botREDACTED/sendMessage",
		},
		{
			name:     "unrelated URL",
			rawURL:   "https://example.com/api/test?bot=unchanged",
			expected: "https://example.com/api/test?bot=unchanged",
		},
		{
			name:     "unrelated bot-like path",
			rawURL:   "https://example.com/botanical/docs",
			expected: "https://example.com/botanical/docs",
		},
		{
			name:     "malformed official URL",
			rawURL:   "https://api.telegram.org/bot" + fakeTelegramToken + "/getUpdates%zz",
			expected: "https://api.telegram.org/botREDACTED/getUpdates%zz",
		},
		{
			name:     "empty URL",
			rawURL:   "",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, redactTelegramBotURL(tc.rawURL))
		})
	}
}

func TestTelegramHTTPClientSuccessfulRequestIsTransparent(t *testing.T) {
	expected := &http.Response{StatusCode: http.StatusNoContent}
	client := &telegramHTTPClient{client: httpDoerFunc(func(_ *http.Request) (*http.Response, error) {
		return expected, nil
	})}
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot"+fakeTelegramToken+"/sendMessage", nil)
	require.NoError(t, err)

	resp, err := client.Do(req)

	require.NoError(t, err)
	assert.Same(t, expected, resp)
}

func TestTelegramClientConstructorReceivesSanitizedError(t *testing.T) {
	underlying := errors.New("network unavailable")
	client := &telegramHTTPClient{client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: underlying}
	})}

	_, err := tbapi.NewBotAPIWithClient(fakeTelegramToken, tbapi.APIEndpoint, client)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), fakeTelegramToken)
	assert.Contains(t, err.Error(), "/botREDACTED/getMe")
	assert.True(t, errors.Is(err, underlying))
	var gotURL *url.Error
	assert.True(t, errors.As(err, &gotURL))
}

func TestTelegramPollingLoggerDoesNotReceiveToken(t *testing.T) {
	logger := newCaptureLogger()
	require.NoError(t, tbapi.SetLogger(logger))
	t.Cleanup(func() {
		require.NoError(t, tbapi.SetLogger(log.New(os.Stderr, "", log.LstdFlags)))
	})

	underlying := errors.New("network unavailable")
	calls := 0
	client := &telegramHTTPClient{client: httpDoerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			body := `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"test_bot"}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: underlying}
	})}

	bot, err := tbapi.NewBotAPIWithClient(fakeTelegramToken, tbapi.APIEndpoint, client)
	require.NoError(t, err)
	bot.GetUpdatesChan(tbapi.NewUpdate(0))
	t.Cleanup(bot.StopReceivingUpdates)

	logs := logger.waitFor(t, "/botREDACTED/getUpdates")
	assert.NotContains(t, logs, fakeTelegramToken)
	assert.Contains(t, logs, "Failed to get updates")
}

type captureLogger struct {
	mu     sync.Mutex
	lines  []string
	notify chan struct{}
}

func newCaptureLogger() *captureLogger {
	return &captureLogger{notify: make(chan struct{}, 1)}
}

func (l *captureLogger) Println(values ...interface{}) {
	l.record(fmt.Sprintln(values...))
}

func (l *captureLogger) Printf(format string, values ...interface{}) {
	l.record(fmt.Sprintf(format, values...))
}

func (l *captureLogger) record(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *captureLogger) waitFor(t *testing.T, value string) string {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		l.mu.Lock()
		output := strings.Join(l.lines, "")
		l.mu.Unlock()
		if strings.Contains(output, value) {
			return output
		}

		select {
		case <-l.notify:
		case <-timer.C:
			t.Fatalf("timed out waiting for sanitized Telegram log; output=%q", output)
		}
	}
}
