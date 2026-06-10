package bot

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const defaultLowTextThreshold = 12

// ContentExtractionConfig controls the optional normalized content foundation.
// All toggles are disabled by default to preserve the current production path.
type ContentExtractionConfig struct {
	Enabled               bool
	LogDebug              bool
	IncludeQuote          bool
	IncludeReply          bool
	IncludePreview        bool
	IncludeForwarded      bool
	UseFingerprintForDups bool
	LowTextThreshold      int
}

// NormalizedMessageContent is a detector-facing view of Telegram message content.
// It intentionally does not perform OCR or persist raw payloads.
type NormalizedMessageContent struct {
	RawText          string
	Caption          string
	ForwardedText    string
	ForwardedCaption string
	MediaCaption     string
	PreviewText      string
	CombinedText     string
	HasMedia         bool
	MediaTypes       []string
	HasButtons       bool
	HasLinks         bool
	HasMentions      bool
	IsForwarded      bool
	IsLowText        bool
	IsEmptyText      bool
	ContentSources   []string
	FingerprintBase  string
}

// NormalizeMessageContent extracts a stable content view from a bot message.
func NormalizeMessageContent(msg Message, cfg ContentExtractionConfig) NormalizedMessageContent {
	res := NormalizedMessageContent{
		RawText:     msg.Text,
		HasButtons:  msg.WithKeyboard,
		IsForwarded: msg.WithForward,
		IsEmptyText: strings.TrimSpace(msg.Text) == "",
	}

	if msg.Image != nil {
		res.HasMedia = true
		res.MediaTypes = append(res.MediaTypes, "photo")
		res.Caption = msg.Image.Caption
		res.MediaCaption = msg.Image.Caption
	}
	if msg.WithVideo || msg.WithVideoNote {
		res.HasMedia = true
		res.MediaTypes = append(res.MediaTypes, "video")
	}
	if msg.WithAudio {
		res.HasMedia = true
		res.MediaTypes = append(res.MediaTypes, "audio")
	}
	res.HasLinks, res.HasMentions = hasLinkMentionEntities(msg)

	var parts []string
	addPart := func(source, text string) {
		text = normalizeContentText(text)
		if text == "" || containsContentPart(parts, text) {
			return
		}
		parts = append(parts, text)
		res.ContentSources = append(res.ContentSources, source)
	}

	addPart("raw_text", msg.Text)
	if msg.Image != nil {
		addPart("media_caption", msg.Image.Caption)
	}
	if cfg.IncludeQuote && msg.Quote != "" {
		addPart("quote", msg.Quote)
	} else if cfg.IncludeReply {
		addPart("reply_text", msg.ReplyTo.Text)
	}

	res.CombinedText = strings.Join(parts, "\n")
	res.FingerprintBase = buildFingerprintBase(res)

	threshold := cfg.LowTextThreshold
	if threshold <= 0 {
		threshold = defaultLowTextThreshold
	}
	res.IsLowText = res.CombinedText == "" || len([]rune(res.CombinedText)) < threshold

	return res
}

func normalizeContentText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func containsContentPart(parts []string, text string) bool {
	for _, part := range parts {
		if part == text {
			return true
		}
	}
	return false
}

func buildFingerprintBase(content NormalizedMessageContent) string {
	if content.CombinedText != "" {
		return content.CombinedText
	}

	markers := make([]string, 0, 4+len(content.MediaTypes))
	for _, mediaType := range content.MediaTypes {
		markers = append(markers, "media:"+mediaType)
	}
	if content.IsForwarded {
		markers = append(markers, "forwarded")
	}
	if content.HasButtons {
		markers = append(markers, "buttons")
	}
	if content.HasLinks {
		markers = append(markers, "links")
	}
	if len(markers) == 0 {
		return ""
	}

	base := "[" + strings.Join(markers, "][") + "]"
	sum := sha256.Sum256([]byte(base))
	return fmt.Sprintf("%s:%x", base, sum[:4])
}

func hasLinkMentionEntities(msg Message) (hasLinks bool, hasMentions bool) {
	check := func(entities *[]Entity) {
		if entities == nil {
			return
		}
		for _, entity := range *entities {
			switch entity.Type {
			case "mention", "text_mention":
				hasMentions = true
			case "url", "text_link":
				hasLinks = true
			}
		}
	}
	check(msg.Entities)
	if msg.Image != nil {
		check(msg.Image.Entities)
	}
	return hasLinks, hasMentions
}
