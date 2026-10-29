package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
)

const (
	telegramStartupAttempts = 5
	telegramStartupRequest  = 15 * time.Second
	telegramStartupBudget   = 90 * time.Second
)

// Only the synchronous constructor uses this policy. Its client is detached on
// success, before the listener (and its recovery worker) can start.
type telegramStartup struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
	logf func(string, ...any)
}

func initializeTelegram(ctx context.Context, token, endpoint string, client *telegramHTTPClient) (*tbapi.BotAPI, error) {
	s := telegramStartup{now: time.Now, wait: waitTelegramStartup, logf: log.Printf}
	return s.initialize(ctx, token, endpoint, client)
}

func waitTelegramStartup(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("telegram initialization wait: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// The error and logs intentionally retain no underlying URL, response text or
// transport error: even a gateway or custom transport can echo credentials.
func (s telegramStartup) failure(attempt int, category, outcome string) error {
	s.logf("[ERROR] Telegram initialization: attempt=%d/%d category=%s outcome=%s",
		attempt, telegramStartupAttempts, category, outcome)
	return fmt.Errorf("telegram initialization: attempt=%d/%d category=%s outcome=%s",
		attempt, telegramStartupAttempts, category, outcome)
}

func (s telegramStartup) contextFailure(attempt int, err error) error {
	category, outcome := "context", "canceled"
	cause := context.Canceled
	if errors.Is(err, context.DeadlineExceeded) {
		category, outcome, cause = "deadline", "budget_exhausted", context.DeadlineExceeded
	}
	return fmt.Errorf("%v: %w", s.failure(attempt, category, outcome), cause)
}

func (s telegramStartup) initialize(ctx context.Context, token, endpoint string,
	client *telegramHTTPClient) (*tbapi.BotAPI, error) {
	if !validStartupToken(token) {
		return nil, s.failure(0, "invalid_token", "permanent_failure")
	}
	bounded, stop := context.WithTimeout(ctx, telegramStartupBudget)
	defer stop()
	end := s.now().Add(telegramStartupBudget)
	if deadline, ok := ctx.Deadline(); ok {
		end = s.now().Add(min(telegramStartupBudget, time.Until(deadline)))
	}
	for attempt := 1; attempt <= telegramStartupAttempts; attempt++ {
		if err := bounded.Err(); err != nil {
			return nil, s.contextFailure(attempt-1, err)
		}
		if end.Sub(s.now()) < telegramStartupRequest {
			return nil, s.failure(attempt-1, "budget", "budget_exhausted")
		}
		attemptCtx, cancel := context.WithTimeout(bounded, telegramStartupRequest)
		attemptCtx = context.WithValue(attemptCtx, telegramRouteRootKey{}, bounded)
		// Preserve the durable cooldown gate. Capture raw HTTP statuses before
		// the regular client or the dependency can discard them during decoding.
		startupClient := *client
		startupClient.client = &telegramStartupStatusClient{client: client.client, now: s.now, pause: client.pause}
		constructorClient := telegramStartupContextClient{ctx: attemptCtx, client: &startupClient}
		api, err := tbapi.NewBotAPIWithClient(token, endpoint, constructorClient)
		cancel() // GetMe has decoded and closed its response, including on failure.
		if bounded.Err() != nil {
			return nil, s.contextFailure(attempt, bounded.Err())
		}
		if err == nil {
			if !s.now().Before(end) {
				return nil, s.failure(attempt, "budget", "budget_exhausted")
			}
			api.Client = client // Never retain the canceled startup context.
			s.logf("[INFO] Telegram initialization: attempt=%d/%d outcome=success", attempt, telegramStartupAttempts)
			return api, nil
		}
		category, retryAfter, retry := classifyTelegramStartup(err)
		if errors.Is(err, context.Canceled) {
			return nil, s.contextFailure(attempt, context.Canceled)
		}
		if !retry {
			return nil, s.failure(attempt, category, "permanent_failure")
		}
		if attempt == telegramStartupAttempts {
			return nil, s.failure(attempt, category, "attempts_exhausted")
		}
		delay := max(time.Second<<(attempt-1), retryAfter)
		// Reserve a full 15-second next attempt, including for rate limits.
		if delay > end.Sub(s.now())-telegramStartupRequest {
			return nil, s.failure(attempt, category, "budget_exhausted")
		}
		s.logf("[WARN] Telegram initialization: attempt=%d/%d category=%s retry_in=%s",
			attempt, telegramStartupAttempts, category, delay)
		if err := s.wait(bounded, delay); err != nil {
			return nil, s.contextFailure(attempt, err)
		}
	}
	return nil, s.failure(telegramStartupAttempts, "budget", "attempts_exhausted")
}

func validStartupToken(token string) bool {
	id, secret, ok := strings.Cut(token, ":")
	if !ok || secret == "" {
		return false
	}
	botID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || botID == 0 {
		return false
	}
	for _, ch := range secret {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

type telegramStartupContextClient struct {
	ctx    context.Context
	client httpDoer
}

func (c telegramStartupContextClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req.Clone(c.ctx))
	if err != nil {
		return resp, fmt.Errorf("telegram initialization request: %w", err)
	}
	return resp, nil
}

type telegramStartupStatusClient struct {
	client httpDoer
	now    func() time.Time
	pause  func(context.Context, time.Time) error
}

func (c *telegramStartupStatusClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("telegram initialization transport: %w", err)
	}
	if resp == nil || resp.Body == nil {
		return nil, &telegramStartupPermanentError{category: "invalid_response"}
	}
	if resp.StatusCode < 400 {
		return resp, nil // The dependency owns and closes successful bodies.
	}
	defer resp.Body.Close()
	retry := time.Duration(0)
	if resp.StatusCode == http.StatusTooManyRequests {
		// Never silently truncate retry_after. Unreadable/oversized 429 bodies
		// fail closed instead of guessing a shorter delay.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		var body tbapi.APIResponse
		if readErr != nil || len(raw) > 65536 || json.Unmarshal(raw, &body) != nil {
			return nil, &telegramStartupPermanentError{category: "invalid_rate_limit_response"}
		}
		if body.Parameters != nil && body.Parameters.RetryAfter > 0 {
			if int64(body.Parameters.RetryAfter) > int64(time.Duration(1<<63-1)/time.Second) {
				return nil, &telegramStartupPermanentError{category: "invalid_retry_after"}
			}
			retry = startupRetrySeconds(body.Parameters.RetryAfter)
		}
		if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
			if int64(seconds) > int64(time.Duration(1<<63-1)/time.Second) {
				return nil, &telegramStartupPermanentError{category: "invalid_retry_after"}
			}
			retry = max(retry, startupRetrySeconds(seconds))
		} else if until, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			if until.After(c.now().Add(time.Duration(1<<63 - 1))) {
				return nil, &telegramStartupPermanentError{category: "invalid_retry_after"}
			}
			retry = max(retry, until.Sub(c.now()))
		}
		if retry <= 0 {
			return nil, &telegramStartupPermanentError{category: "missing_retry_after"}
		}
		if c.pause != nil {
			persistCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
			defer cancel()
			if e := c.pause(persistCtx, c.now().Add(retry)); e != nil {
				return nil, &telegramStartupPermanentError{category: "cooldown_unavailable"}
			}
		}
	}
	return nil, &telegramStartupHTTPError{code: resp.StatusCode, retry: retry}
}

// Preserve long server delays, including in the durable cooldown. Saturation is
// only for values that cannot be represented by time.Duration; they cannot fit
// any startup attempt and therefore never result in a shortened local wait.
func startupRetrySeconds(seconds int) time.Duration {
	const maxDelay = time.Duration(1<<63 - 1)
	if int64(seconds) > int64(maxDelay/time.Second) {
		return maxDelay
	}
	return time.Duration(seconds) * time.Second
}

type telegramStartupHTTPError struct {
	code  int
	retry time.Duration
}

func (e *telegramStartupHTTPError) Error() string { return "telegram initialization HTTP refusal" }

type telegramStartupPermanentError struct{ category string }

func (e *telegramStartupPermanentError) Error() string {
	return "telegram initialization: " + e.category
}

func classifyTelegramStartup(err error) (category string, retryAfter time.Duration, retry bool) {
	if errors.Is(err, context.Canceled) {
		return "canceled", 0, false
	}
	var permanent *telegramStartupPermanentError
	if errors.As(err, &permanent) {
		return permanent.category, 0, false
	}
	var cooldown cooldownUnavailable
	if errors.As(err, &cooldown) {
		return "cooldown_unavailable", 0, false
	}
	var status *telegramStartupHTTPError
	if errors.As(err, &status) {
		return startupStatusCategory(status.code, status.retry)
	}
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) {
		return startupStatusCategory(apiErr.Code, startupRetrySeconds(apiErr.RetryAfter))
	}
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &certificate) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &record) {
		return "tls_certificate", 0, false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", 0, !dns.IsNotFound && (dns.IsTemporary || dns.IsTimeout)
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout", 0, true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "transport", 0, true
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return "transport", 0, true
	}
	return "permanent", 0, false
}

func startupStatusCategory(code int, retryAfter time.Duration) (string, time.Duration, bool) {
	if code == http.StatusTooManyRequests {
		return "rate_limit", retryAfter, retryAfter > 0
	}
	return fmt.Sprintf("http_%d", code), 0, code >= 500 && code <= 599
}
