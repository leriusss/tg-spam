package events

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/umputun/tg-spam/app/moderation"
)

const unparsedDetailReason = "Сохранённую причину не удалось достоверно распознать. " +
	"Безопасный сохранённый текст доступен в технических данных."

var (
	savedCheckPattern = regexp.MustCompile(`^([^:]+): (spam|ham|error),(?: (.*))?$`)
	detailCountPair   = regexp.MustCompile(`^(\d+)/(\d+)$`)
	detailSimilarity  = regexp.MustCompile(`^(\d+(?:\.\d+)?)/(\d+(?:\.\d+)?)$`)
	detailClassifier  = regexp.MustCompile(`^probability of spam: (\d+(?:\.\d+)?)%(?:[;,] threshold[:=] (\d+(?:\.\d+)?)%)?$`)
	detailDuplicate   = regexp.MustCompile(`^message repeated (\d+) times in (.+)$`)
	detailBurst       = regexp.MustCompile(`^[^:\n]+: repeated (\d+) times in (\d+)s$`)
	detailMediaLength = regexp.MustCompile(`^(?:image|video|audio) with short text \((\d+) chars\)$`)
	detailSpacing     = regexp.MustCompile(`^abnormal \(ratio: (\d+(?:\.\d+)?), short: (\d+(?:\.\d+)?)%\)$`)
	detailUserSymbol  = regexp.MustCompile(`^username contains prohibited symbol '(.)'$`)
)

// Only two persisted verdict formats are understood: Response.String and the
// positive-only envelope written by hanModerationReason. A check name alone is
// not evidence of spam. Neither locator nor current thresholds are consulted.
func savedDetailReason(line string, c moderation.Card) string {
	name, details, status := "", "", ""
	for _, prefix := range []string{"Независимое основание модерации: ", "Независимое основание: "} {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		var found bool
		name, details, found = strings.Cut(rest, " —")
		details = strings.TrimPrefix(details, " ")
		if found {
			status = "spam"
		}
		break
	}
	if status == "" {
		if match := savedCheckPattern.FindStringSubmatch(strings.TrimPrefix(line, "- ")); len(match) == 4 {
			name, status, details = match[1], match[2], match[3]
		}
	}
	label := detailCheckLabel(name)
	if status == "" || label == "" {
		return unparsedDetailReason
	}
	if status != "spam" || detailCheckFailure(name, details) {
		return "Проверка «" + label + "»: положительный результат не подтверждён; это не основание спама."
	}
	return positiveDetailReason(name, details, label, c)
}

func detailCheckLabel(name string) string {
	return map[string]string{
		"emoji": "количество эмодзи", "classifier": "классификатор", "similarity": "сходство с примерами спама",
		"cas": "CAS", "duplicate": "повторяющиеся сообщения", "burst": "повторяющиеся сообщения",
		"повторяющиеся сообщения (burst)": "повторяющиеся сообщения",
		"stopword": "стоп-слово", "stop words": "стоп-слово", "стоп-слово": "стоп-слово",
		"external inline button": "внешняя кнопка со ссылкой", "внешняя кнопка со ссылкой": "внешняя кнопка со ссылкой",
		"links": "количество ссылок", "mentions": "количество упоминаний", "link-only": "только ссылки",
		"images": "изображение без достаточного текста", "videos": "видео без достаточного текста",
		"audio": "аудио без достаточного текста", "contact": "контакт без текста", "forward": "пересылка",
		"keyboard": "кнопки в сообщении", "giveaway": "розыгрыш", "username-symbols": "символы в имени пользователя",
		"multi-lingual": "смешение письменностей внутри слов", "word-spacing": "необычные пробелы",
		"confirmed exact text": "точное совпадение со спам-фразой", "openai": "проверка моделью OpenAI",
	}[name]
}

func detailCheckFailure(name, details string) bool {
	// These are explicit detector failure/negative formats, not searches for
	// words inside arbitrary CAS descriptions or matched stopword text.
	if name == "stopword" || name == "stop words" || name == "стоп-слово" || name == "confirmed exact text" {
		return false // details is a matched literal, not an error field
	}
	for _, prefix := range []string{"failed to ", "invalid user id", "OpenAI error:", "error:", "classifier spam suppressed:"} {
		if strings.HasPrefix(details, prefix) {
			return true
		}
	}
	return details == "check disabled" || details == "not found" || details == "record not found"
}

func detailNumber(s string, maxValue float64) bool {
	n, err := strconv.ParseFloat(s, 64)
	return err == nil && n >= 0 && n <= maxValue
}

func positiveDetailReason(name, details, label string, c moderation.Card) string {
	switch name {
	case "emoji", "multi-lingual", "links", "mentions":
		pair := details
		if name == "links" || name == "mentions" {
			pair = strings.TrimPrefix(pair, "too many "+name+" ")
		}
		if match := detailCountPair.FindStringSubmatch(pair); len(match) == 3 &&
			detailNumber(match[1], 1e15) && detailNumber(match[2], 1e15) {
			count, _ := strconv.ParseInt(match[1], 10, 64)
			limit, _ := strconv.ParseInt(match[2], 10, 64)
			if count <= limit {
				return unparsedDetailReason
			}
			if name == "emoji" {
				return fmt.Sprintf("Превышен лимит эмодзи: обнаружено %s, лимит — %s.", match[1], match[2])
			}
			return fmt.Sprintf("Сработала проверка «%s»: обнаружено %s, лимит — %s.", label, match[1], match[2])
		}
		return "Сработала проверка «" + label + "». Количество и лимит не сохранены или не распознаны."
	case "classifier":
		text := "Классификатор определил спам."
		if match := detailClassifier.FindStringSubmatch(details); len(match) == 3 && detailNumber(match[1], 100) {
			text += " Вероятность спама: " + match[1] + "%."
			if match[2] != "" && detailNumber(match[2], 100) {
				return text + " Порог: " + match[2] + "%."
			}
			return text + " Порог не сохранён или не распознан."
		}
		if strings.HasPrefix(details, "probability of ham:") || strings.HasPrefix(details, "probability of spam:") {
			return unparsedDetailReason
		}
		return text + " Вероятность и порог не сохранены или не распознаны."
	case "similarity":
		text := "Обнаружено сходство с примерами спама."
		if match := detailSimilarity.FindStringSubmatch(details); len(match) == 3 &&
			detailNumber(match[1], 1) && detailNumber(match[2], 1) {
			return fmt.Sprintf("%s Значение: %s; порог: %s.", text, match[1], match[2])
		}
		return text + " Значение и порог не сохранены или не распознаны."
	case "cas":
		if detailActor(c) == "Автор не установлен" {
			return "CAS: сохранён положительный результат, но числовая цель проверки не установлена."
		}
		return "CAS: числовая цель найдена в антиспам-базе; сохранён положительный результат."
	case "duplicate", "burst", "повторяющиеся сообщения (burst)":
		text := "Отдельное автоматическое правило: серия повторяющихся сообщений."
		if match := detailDuplicate.FindStringSubmatch(details); len(match) == 3 && detailNumber(match[1], 1e15) {
			text += " Сохранённое число повторений: " + match[1] + "."
			if duration, err := time.ParseDuration(match[2]); err == nil && duration >= 0 {
				text += fmt.Sprintf(" Период повторов: %g с.", duration.Seconds())
			} else if match[2] == "instantly" {
				text += " Повторы зафиксированы одновременно."
			}
		} else if match := detailBurst.FindStringSubmatch(details); len(match) == 3 &&
			detailNumber(match[1], 1e15) && detailNumber(match[2], 1e15) {
			text += fmt.Sprintf(" Сохранённое число повторений: %s; окно: %s с.", match[1], match[2])
		}
		return text
	case "stopword", "stop words", "стоп-слово", "external inline button", "внешняя кнопка со ссылкой":
		text := "Отдельное автоматическое правило: " + label + "."
		if label == "стоп-слово" && details != "" {
			text += " Найденное выражение: «" + details + "»."
		}
		return text
	case "images", "videos", "audio":
		text := "Сработала проверка «" + label + "»."
		if match := detailMediaLength.FindStringSubmatch(details); len(match) == 2 && detailNumber(match[1], 1e15) {
			text += " Длина подписи: " + match[1] + " символов."
		}
		return text
	case "word-spacing":
		text := "Обнаружены необычные пробелы и короткие фрагменты слов."
		if match := detailSpacing.FindStringSubmatch(details); len(match) == 3 &&
			detailNumber(match[1], 1) && detailNumber(match[2], 100) {
			text += " Доля пробелов: " + match[1] + "; коротких слов: " + match[2] + "%."
		}
		return text
	case "confirmed exact text":
		return "Обнаружено точное совпадение со спам-фразой: «" + details + "»."
	case "username-symbols":
		text := "Имя пользователя содержит запрещённый настройкой символ."
		if match := detailUserSymbol.FindStringSubmatch(details); len(match) == 2 {
			text += " Символ: «" + match[1] + "»."
		}
		return text
	case "openai":
		return "Модель OpenAI определила сообщение как спам. Сохранённое объяснение: «" + details + "»."
	default:
		return "Сработала проверка «" + label + "»."
	}
}
