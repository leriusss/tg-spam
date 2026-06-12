package bot

import (
	"strings"
	"sync"
	"time"
)

const (
	burstSimilarityExact       = "exact"
	defaultBurstWindowSeconds  = 30
	defaultBurstThreshold      = 3
	defaultBurstMaxUsers       = 1000
	defaultBurstMaxMessages    = 10
	minBurstFingerprintRunes   = 4
	burstReasonExactRepetition = "burst_exact_repetition"
)

// BurstConfig controls the disabled-first burst detector foundation.
type BurstConfig struct {
	Enabled            bool
	WindowSeconds      int
	Threshold          int
	Similarity         string
	MaxUsers           int
	MaxMessagesPerUser int
	CleanupEnabled     bool
	LogDebug           bool
}

// BurstEntry is one bounded in-memory observation for a chat/user bucket.
type BurstEntry struct {
	Fingerprint string
	Timestamp   time.Time
	MessageID   int
}

// BurstDecision is the detector result for one checked message.
type BurstDecision struct {
	Spam           bool
	Reason         string
	Count          int
	WindowSeconds  int
	ExtraDeleteIDs []int
}

type burstKey struct {
	chatID          int64
	effectiveUserID int64
}

// BurstDetector detects repeated exact normalized messages from the same sender in the same chat.
type BurstDetector struct {
	mu      sync.Mutex
	config  BurstConfig
	buckets map[burstKey][]BurstEntry
	clock   func() time.Time
}

// NewBurstDetector creates a burst detector with safe defaults.
func NewBurstDetector(config BurstConfig) *BurstDetector {
	config = normalizeBurstConfig(config)
	return &BurstDetector{
		config:  config,
		buckets: map[burstKey][]BurstEntry{},
		clock:   time.Now,
	}
}

// SetClock replaces the detector clock used when Check receives a zero time.
func (d *BurstDetector) SetClock(clock func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if clock == nil {
		d.clock = time.Now
		return
	}
	d.clock = clock
}

// Config returns a copy of the normalized detector config.
func (d *BurstDetector) Config() BurstConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.config
}

// Check records an eligible message and reports an exact same-chat/same-sender burst.
func (d *BurstDetector) Check(chatID, effectiveUserID int64, messageID int, text string, now time.Time) BurstDecision {
	if d == nil {
		return BurstDecision{}
	}

	fingerprint := burstFingerprint(text)

	d.mu.Lock()
	defer d.mu.Unlock()

	config := d.config
	if !config.Enabled || fingerprint == "" {
		return BurstDecision{WindowSeconds: config.WindowSeconds}
	}
	if now.IsZero() {
		now = d.clock()
	}

	key := burstKey{chatID: chatID, effectiveUserID: effectiveUserID}
	d.pruneExpiredLocked(now)
	if _, ok := d.buckets[key]; !ok {
		d.enforceMaxUsersLocked(key)
	}

	entries := pruneBurstEntries(d.buckets[key], now, config.window())
	entries = append(entries, BurstEntry{Fingerprint: fingerprint, Timestamp: now, MessageID: messageID})
	if len(entries) > config.MaxMessagesPerUser {
		entries = entries[len(entries)-config.MaxMessagesPerUser:]
	}
	d.buckets[key] = entries

	count := 0
	matchingIDs := make([]int, 0, config.MaxMessagesPerUser)
	for _, entry := range entries {
		if entry.Fingerprint != fingerprint {
			continue
		}
		count++
		if entry.MessageID != 0 && entry.MessageID != messageID {
			matchingIDs = append(matchingIDs, entry.MessageID)
		}
	}

	decision := BurstDecision{Count: count, WindowSeconds: config.WindowSeconds}
	if count < config.Threshold {
		return decision
	}

	decision.Spam = true
	decision.Reason = burstReasonExactRepetition
	if config.CleanupEnabled {
		if len(matchingIDs) > config.MaxMessagesPerUser {
			matchingIDs = matchingIDs[len(matchingIDs)-config.MaxMessagesPerUser:]
		}
		decision.ExtraDeleteIDs = append(decision.ExtraDeleteIDs, matchingIDs...)
	}
	return decision
}

func normalizeBurstConfig(config BurstConfig) BurstConfig {
	if config.WindowSeconds <= 0 {
		config.WindowSeconds = defaultBurstWindowSeconds
	}
	if config.Threshold <= 0 {
		config.Threshold = defaultBurstThreshold
	}
	if config.Similarity == "" {
		config.Similarity = burstSimilarityExact
	}
	if config.Similarity != burstSimilarityExact {
		config.Enabled = false
	}
	if config.MaxUsers <= 0 {
		config.MaxUsers = defaultBurstMaxUsers
	}
	if config.MaxMessagesPerUser <= 0 {
		config.MaxMessagesPerUser = defaultBurstMaxMessages
	}
	return config
}

func (c BurstConfig) window() time.Duration {
	return time.Duration(c.WindowSeconds) * time.Second
}

func burstFingerprint(text string) string {
	fp := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if fp == "" {
		return ""
	}
	if len([]rune(fp)) < minBurstFingerprintRunes || isCommonBurstFingerprint(fp) {
		return ""
	}
	return fp
}

func isCommonBurstFingerprint(fp string) bool {
	switch fp {
	case "ok", "okay", "yes", "no", "hi", "hey", "hello", "thanks", "thank you", "lol", "+", "++", ".":
		return true
	default:
		return false
	}
}

func pruneBurstEntries(entries []BurstEntry, now time.Time, window time.Duration) []BurstEntry {
	if len(entries) == 0 {
		return entries
	}
	cutoff := now.Add(-window)
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Timestamp.Before(cutoff) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func (d *BurstDetector) pruneExpiredLocked(now time.Time) {
	for key, entries := range d.buckets {
		entries = pruneBurstEntries(entries, now, d.config.window())
		if len(entries) == 0 {
			delete(d.buckets, key)
			continue
		}
		d.buckets[key] = entries
	}
}

func (d *BurstDetector) enforceMaxUsersLocked(incoming burstKey) {
	if d.config.MaxUsers <= 0 {
		return
	}
	for len(d.buckets) >= d.config.MaxUsers {
		var evictKey burstKey
		var evictTime time.Time
		first := true
		for key, entries := range d.buckets {
			if key == incoming || len(entries) == 0 {
				continue
			}
			last := newestBurstEntryTime(entries)
			if first || last.Before(evictTime) {
				first = false
				evictKey = key
				evictTime = last
			}
		}
		if first {
			return
		}
		delete(d.buckets, evictKey)
	}
}

func newestBurstEntryTime(entries []BurstEntry) time.Time {
	newest := entries[0].Timestamp
	for _, entry := range entries[1:] {
		if entry.Timestamp.After(newest) {
			newest = entry.Timestamp
		}
	}
	return newest
}
