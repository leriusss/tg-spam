package bot

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/umputun/tg-spam/lib/spamcheck"
)

// HanConfig controls a script restriction, not language identification.
type HanConfig struct {
	Enabled  bool
	Ratio    float64
	MinCount int
}

// Validate rejects thresholds that cannot express a predominant script.
func (c HanConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if math.IsNaN(c.Ratio) || math.IsInf(c.Ratio, 0) || c.Ratio <= 0.5 || c.Ratio > 1 || c.MinCount < 2 {
		return fmt.Errorf("han rule requires ratio in (0.5, 1] and min-count >= 2")
	}
	return nil
}

var hanURL = regexp.MustCompile(`(?i)(?:https?://|tg://|www\.)[^\s<>\[\]()]+`)

// HanCounts counts Han letters among all letters, excluding literal URLs.
func HanCounts(text string) (han, letters int) {
	for _, r := range hanURL.ReplaceAllString(text, " ") {
		if unicode.IsLetter(r) {
			letters++
			if unicode.Is(unicode.Han, r) {
				han++
			}
		}
	}
	return han, letters
}

// HanOwnText excludes Telegram block quotes and URL entities using UTF-16 offsets.
func HanOwnText(text string, entities *[]Entity) string {
	if entities == nil {
		return text
	}
	units := utf16.Encode([]rune(text))
	for _, e := range *entities {
		if e.Type != "url" && e.Type != "blockquote" && e.Type != "expandable_blockquote" {
			continue
		}
		if e.Offset < 0 || e.Length <= 0 || e.Offset > len(units) || e.Length > len(units)-e.Offset {
			continue
		}
		for i := e.Offset; i < e.Offset+e.Length; i++ {
			units[i] = ' '
		}
	}
	return string(utf16.Decode(units))
}

func (s *SpamFilter) hanResponse(msg Message, userID string) (Response, bool) {
	cfg := s.params.Han
	if !cfg.Enabled || cfg.Validate() != nil || msg.WithForward || s.Detector.IsExplicitTrustedUser(userID) {
		return Response{}, false
	}
	text := HanOwnText(msg.Text, msg.Entities)
	if msg.HanText != nil {
		text = *msg.HanText
	} else if strings.TrimSpace(text) == "" && msg.Image != nil {
		text = HanOwnText(msg.Image.Caption, msg.Image.Entities)
	}
	han, letters := HanCounts(text)
	if han < cfg.MinCount || letters == 0 || float64(han)/float64(letters) < cfg.Ratio {
		return Response{}, false
	}
	reason := fmt.Sprintf("Преобладает письменность Han: %d из %d букв (%.1f%%); "+
		"порог %.1f%%, минимум %d. Удаление и блокировка учитываются отдельно; обучение и очистка по Han не выполняются.",
		han, letters, 100*float64(han)/float64(letters), 100*cfg.Ratio, cfg.MinCount)
	return Response{Send: true, ReplyTo: msg.ID, DeleteReplyTo: true, HanMatched: true, HanOnly: true,
		BanInterval: PermanentBanDuration,
		User:        msg.From, ChannelID: msg.SenderChat.ID,
		CheckResults: []spamcheck.Response{{Name: "han script", Spam: true, Details: reason}}}, true
}
