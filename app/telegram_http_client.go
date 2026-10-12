package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
)

const telegramBotTokenMarker = "REDACTED"

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// telegramHTTPClient keeps the default HTTP client behavior while removing bot
// tokens from URL-bearing errors before they reach the Telegram library logger.
type telegramHTTPClient struct {
	client       httpDoer
	recoveryHTTP bool
	cooldown     func(context.Context) (time.Time, error)
	pause        func(context.Context, time.Time) error
}

func newTelegramHTTPClient() *telegramHTTPClient {
	return &telegramHTTPClient{client: &http.Client{}}
}

func (c *telegramHTTPClient) Do(req *http.Request) (*http.Response, error) {
	for c.cooldown != nil {
		until, err := c.cooldown(req.Context())
		if err != nil {
			return nil, cooldownUnavailable{}
		}
		if wait := time.Until(until); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, fmt.Errorf("telegram cooldown wait: %w", req.Context().Err())
			case <-timer.C:
			}
		} else {
			break
		}
	}
	cancel := func() {}
	if c.recoveryHTTP {
		timeout := 15 * time.Second
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			timeout = 90 * time.Second
		}
		var bounded context.Context
		bounded, cancel = context.WithTimeout(req.Context(), timeout)
		req = req.Clone(bounded)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		cancel()
	} else if resp != nil && resp.Body != nil {
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	} else {
		cancel()
	}
	if err == nil && resp != nil && c.recoveryHTTP &&
		(resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 ||
			resp.StatusCode == 503 || resp.StatusCode == 504) {
		// Gateways may send HTML instead of Bot API JSON; preserve the typed HTTP classification.
		var body tbapi.APIResponse
		if resp.Body != nil {
			_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&body)
			_ = resp.Body.Close()
		}
		retry := 0
		if body.Parameters != nil {
			retry = body.Parameters.RetryAfter
		}
		if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > retry {
			retry = seconds
		} else if when, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			seconds = int(math.Ceil(time.Until(when).Seconds()))
			if seconds > retry {
				retry = seconds
			}
		}
		if resp.StatusCode == 429 && retry <= 0 {
			retry = 60
		}
		if resp.StatusCode == 429 && c.pause != nil {
			persistCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			e := c.pause(persistCtx, time.Now().Add(time.Duration(retry)*time.Second))
			cancel()
			if e != nil {
				return nil, cooldownUnavailable{}
			}
		}
		return nil, &tbapi.Error{
			Code:               resp.StatusCode,
			Message:            "Telegram HTTP request refused",
			ResponseParameters: tbapi.ResponseParameters{RetryAfter: retry},
		}
	}
	return resp, sanitizeTelegramHTTPError(err)
}

// Failure of the durable rate limiter fails closed and remains retryable, without exposing SQL errors.
type cooldownUnavailable struct{}

func (cooldownUnavailable) Error() string   { return "durable Telegram cooldown unavailable" }
func (cooldownUnavailable) Timeout() bool   { return false }
func (cooldownUnavailable) Temporary() bool { return true }

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	defer b.cancel()
	err := b.ReadCloser.Close()
	if err != nil {
		return fmt.Errorf("telegram response close: %w", err)
	}
	return nil
}

func sanitizeTelegramHTTPError(err error) error {
	if err == nil {
		return nil
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}

	safeURL := redactTelegramBotURL(urlErr.URL)
	if safeURL == urlErr.URL {
		return err
	}

	safeErr := *urlErr
	safeErr.URL = safeURL
	return &safeErr
}

func redactTelegramBotURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err == nil {
		segments := strings.Split(parsed.Path, "/")
		if redactTelegramTokenSegment(segments, parsed.Hostname() == "api.telegram.org") {
			parsed.Path = strings.Join(segments, "/")
			parsed.RawPath = ""
			return parsed.String()
		}
		return rawURL
	}

	// Invalid escaping can make net/url reject an otherwise recognizable
	// Telegram URL. Use a narrow fallback so a known-sensitive URL is not
	// returned verbatim on the error path.
	return redactMalformedTelegramBotURL(rawURL)
}

func redactTelegramTokenSegment(segments []string, officialHost bool) bool {
	for idx, segment := range segments {
		if !strings.HasPrefix(segment, "bot") || len(segment) == len("bot") {
			continue
		}

		token := strings.TrimPrefix(segment, "bot")
		if !officialHost && !looksLikeTelegramBotToken(token) {
			continue
		}

		segments[idx] = "bot" + telegramBotTokenMarker
		return true
	}
	return false
}

func looksLikeTelegramBotToken(token string) bool {
	id, secret, found := strings.Cut(token, ":")
	if !found || id == "" || secret == "" {
		return false
	}
	for _, char := range id {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func redactMalformedTelegramBotURL(rawURL string) string {
	officialHost := strings.Contains(rawURL, "//api.telegram.org/")
	searchFrom := 0
	for {
		relativeStart := strings.Index(rawURL[searchFrom:], "/bot")
		if relativeStart == -1 {
			return rawURL
		}

		segmentStart := searchFrom + relativeStart + 1
		tokenStart := segmentStart + len("bot")
		segmentEnd := len(rawURL)
		if relativeEnd := strings.IndexAny(rawURL[tokenStart:], "/?#"); relativeEnd >= 0 {
			segmentEnd = tokenStart + relativeEnd
		}

		token := rawURL[tokenStart:segmentEnd]
		if token != "" && (officialHost || looksLikeTelegramBotToken(token)) {
			return rawURL[:segmentStart] + "bot" + telegramBotTokenMarker + rawURL[segmentEnd:]
		}

		searchFrom = tokenStart
		if searchFrom >= len(rawURL) {
			return rawURL
		}
	}
}
