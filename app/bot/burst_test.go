package bot

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBurstDetectorDetectsThreeIdenticalMessages(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	assert.False(t, d.Check(1, 10, 101, " Limited   Time OFFER ", now).Spam)
	assert.False(t, d.Check(1, 10, 102, "limited time offer", now.Add(10*time.Second)).Spam)
	decision := d.Check(1, 10, 103, "LIMITED time offer", now.Add(20*time.Second))

	assert.True(t, decision.Spam)
	assert.Equal(t, burstReasonExactRepetition, decision.Reason)
	assert.Equal(t, 3, decision.Count)
	assert.Equal(t, defaultBurstWindowSeconds, decision.WindowSeconds)
}

func TestBurstDetectorTwoMessagesDoNotDetect(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	decision := d.Check(1, 10, 102, "limited time offer", now.Add(time.Second))

	assert.False(t, decision.Spam)
	assert.Equal(t, 2, decision.Count)
}

func TestBurstDetectorSameTextDifferentUsersDoesNotDetect(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(1, 11, 102, "limited time offer", now.Add(time.Second))
	decision := d.Check(1, 12, 103, "limited time offer", now.Add(2*time.Second))

	assert.False(t, decision.Spam)
	assert.Equal(t, 1, decision.Count)
}

func TestBurstDetectorSameUserDifferentChatsDoesNotDetect(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(2, 10, 102, "limited time offer", now.Add(time.Second))
	decision := d.Check(3, 10, 103, "limited time offer", now.Add(2*time.Second))

	assert.False(t, decision.Spam)
	assert.Equal(t, 1, decision.Count)
}

func TestBurstDetectorPrunesOldEntriesOutsideWindow(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, WindowSeconds: 30})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(1, 10, 102, "limited time offer", now.Add(31*time.Second))
	decision := d.Check(1, 10, 103, "limited time offer", now.Add(32*time.Second))

	assert.False(t, decision.Spam)
	assert.Equal(t, 2, decision.Count)
	assert.Len(t, d.buckets[burstKey{chatID: 1, effectiveUserID: 10}], 2)
}

func TestBurstDetectorMaxMessagesPerUserIsBounded(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, Threshold: 100, MaxMessagesPerUser: 3})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		d.Check(1, 10, 100+i, "limited time offer", now.Add(time.Duration(i)*time.Second))
	}

	entries := d.buckets[burstKey{chatID: 1, effectiveUserID: 10}]
	require.Len(t, entries, 3)
	assert.Equal(t, []int{102, 103, 104}, []int{entries[0].MessageID, entries[1].MessageID, entries[2].MessageID})
}

func TestBurstDetectorMaxUsersIsBounded(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, MaxUsers: 2})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(1, 11, 102, "limited time offer", now.Add(time.Second))
	d.Check(1, 12, 103, "limited time offer", now.Add(2*time.Second))

	assert.Len(t, d.buckets, 2)
	assert.NotContains(t, d.buckets, burstKey{chatID: 1, effectiveUserID: 10})
	assert.Contains(t, d.buckets, burstKey{chatID: 1, effectiveUserID: 11})
	assert.Contains(t, d.buckets, burstKey{chatID: 1, effectiveUserID: 12})
}

func TestBurstDetectorEmptyMessagesDoNotBurst(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		decision := d.Check(1, 10, 100+i, " \n\t ", now.Add(time.Duration(i)*time.Second))
		assert.False(t, decision.Spam)
	}

	assert.Empty(t, d.buckets)
}

func TestBurstDetectorEmptyLikeMessagesDoNotCollapse(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	for i, text := range []string{"", "   ", "\n"} {
		decision := d.Check(1, 10, 100+i, text, now.Add(time.Duration(i)*time.Second))
		assert.False(t, decision.Spam)
	}

	assert.Empty(t, d.buckets)
}

func TestBurstDetectorCleanupDisabledReturnsNoExtraDeleteIDs(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, CleanupEnabled: false})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(1, 10, 102, "limited time offer", now.Add(time.Second))
	decision := d.Check(1, 10, 103, "limited time offer", now.Add(2*time.Second))

	assert.True(t, decision.Spam)
	assert.Empty(t, decision.ExtraDeleteIDs)
}

func TestBurstDetectorCleanupEnabledReturnsBoundedMatchingCandidateIDs(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, CleanupEnabled: true, MaxMessagesPerUser: 4})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	d.Check(1, 10, 101, "limited time offer", now)
	d.Check(1, 10, 102, "different message", now.Add(time.Second))
	d.Check(1, 10, 103, "limited time offer", now.Add(2*time.Second))
	decision := d.Check(1, 10, 104, "limited time offer", now.Add(3*time.Second))

	assert.True(t, decision.Spam)
	assert.Equal(t, []int{101, 103}, decision.ExtraDeleteIDs)
	assert.NotContains(t, decision.ExtraDeleteIDs, 104, "current message is not an extra cleanup candidate")
}

func TestBurstDetectorRejectsNonExactSimilaritySafely(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, Similarity: "fuzzy"})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	assert.False(t, d.Config().Enabled)
	for i := 0; i < 3; i++ {
		decision := d.Check(1, 10, 100+i, "limited time offer", now.Add(time.Duration(i)*time.Second))
		assert.False(t, decision.Spam)
	}
	assert.Empty(t, d.buckets)
}

func TestBurstDetectorParallelCalls(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true, Threshold: 1000, MaxMessagesPerUser: 1000})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Check(1, 10, 1000+i, "limited time offer", now.Add(time.Duration(i)*time.Millisecond))
		}(i)
	}
	wg.Wait()

	assert.Len(t, d.buckets[burstKey{chatID: 1, effectiveUserID: 10}], 100)
}

func TestBurstDetectorUsesInjectedClockWhenNowIsZero(t *testing.T) {
	d := NewBurstDetector(BurstConfig{Enabled: true})
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	d.SetClock(func() time.Time { return now })

	d.Check(1, 10, 101, "limited time offer", time.Time{})

	entries := d.buckets[burstKey{chatID: 1, effectiveUserID: 10}]
	require.Len(t, entries, 1)
	assert.Equal(t, now, entries[0].Timestamp)
}
