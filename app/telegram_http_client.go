package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const telegramBotTokenMarker = "REDACTED"

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// telegramHTTPClient keeps the default HTTP client behavior while removing bot
// tokens from URL-bearing errors before they reach the Telegram library logger.
type telegramHTTPClient struct {
	client httpDoer
}

func newTelegramHTTPClient() *telegramHTTPClient {
	return &telegramHTTPClient{client: &http.Client{}}
}

func (c *telegramHTTPClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	return resp, sanitizeTelegramHTTPError(err)
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
