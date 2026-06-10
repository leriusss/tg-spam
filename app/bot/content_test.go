package bot

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeMessageContentPlainText(t *testing.T) {
	content := NormalizeMessageContent(Message{Text: " hello   world "}, ContentExtractionConfig{})

	assert.Equal(t, "hello world", content.CombinedText)
	assert.Equal(t, " hello   world ", content.RawText)
	assert.Equal(t, []string{"raw_text"}, content.ContentSources)
	assert.Equal(t, "hello world", content.FingerprintBase)
	assert.False(t, content.IsEmptyText)
	assert.True(t, content.IsLowText)
}

func TestNormalizeMessageContentImageCaption(t *testing.T) {
	content := NormalizeMessageContent(Message{
		Text:  "visible text",
		Image: &Image{FileID: "photo-id", Caption: "caption text"},
	}, ContentExtractionConfig{})

	assert.Equal(t, "visible text\ncaption text", content.CombinedText)
	assert.Equal(t, "caption text", content.Caption)
	assert.Equal(t, "caption text", content.MediaCaption)
	assert.True(t, content.HasMedia)
	assert.Equal(t, []string{"photo"}, content.MediaTypes)
	assert.Equal(t, []string{"raw_text", "media_caption"}, content.ContentSources)
}

func TestNormalizeMessageContentDoesNotDuplicateCaption(t *testing.T) {
	content := NormalizeMessageContent(Message{
		Text:  "caption text",
		Image: &Image{FileID: "photo-id", Caption: "caption text"},
	}, ContentExtractionConfig{})

	assert.Equal(t, "caption text", content.CombinedText)
	assert.Equal(t, []string{"raw_text"}, content.ContentSources)
}

func TestNormalizeMessageContentQuotePrecedesReply(t *testing.T) {
	msg := Message{Text: "main text", Quote: "quoted text"}
	msg.ReplyTo.Text = "reply text"

	content := NormalizeMessageContent(msg, ContentExtractionConfig{IncludeQuote: true, IncludeReply: true})

	assert.Equal(t, "main text\nquoted text", content.CombinedText)
	assert.Equal(t, []string{"raw_text", "quote"}, content.ContentSources)
}

func TestNormalizeMessageContentReplyWhenQuoteAbsent(t *testing.T) {
	msg := Message{Text: "main text"}
	msg.ReplyTo.Text = "reply text"

	content := NormalizeMessageContent(msg, ContentExtractionConfig{IncludeQuote: true, IncludeReply: true})

	assert.Equal(t, "main text\nreply text", content.CombinedText)
	assert.Equal(t, []string{"raw_text", "reply_text"}, content.ContentSources)
}

func TestNormalizeMessageContentMediaOnlyFallbackFingerprint(t *testing.T) {
	content := NormalizeMessageContent(Message{
		Image:        &Image{FileID: "photo-id"},
		WithForward:  true,
		WithKeyboard: true,
	}, ContentExtractionConfig{})

	assert.Empty(t, content.CombinedText)
	assert.True(t, content.HasMedia)
	assert.True(t, content.HasButtons)
	assert.True(t, content.IsForwarded)
	assert.True(t, content.IsEmptyText)
	assert.True(t, content.IsLowText)
	assert.True(t, strings.HasPrefix(content.FingerprintBase, "[media:photo][forwarded][buttons]:"))
}

func TestNormalizeMessageContentEntityFlags(t *testing.T) {
	entities := []Entity{{Type: "mention"}}
	imageEntities := []Entity{{Type: "text_link", URL: "https://example.com"}}

	content := NormalizeMessageContent(Message{
		Text:     "@user",
		Entities: &entities,
		Image:    &Image{FileID: "photo-id", Entities: &imageEntities},
	}, ContentExtractionConfig{})

	assert.True(t, content.HasMentions)
	assert.True(t, content.HasLinks)
}
