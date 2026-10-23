package tgspam

import (
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"testing"
	"time"
)

func TestSuppressHamRecordingPreservesBehavioralChecks(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		d := NewDetector(Config{FirstMessageOnly: true, MinMsgLen: 1, MaxAllowedEmoji: -1, HistorySize: 10})
		d.DuplicateDetection.Threshold = 3
		d.duplicateDetector = newDuplicateDetector(3, time.Minute)
		for i := 1; i <= 3; i++ {
			spam, checks := d.Check(spamcheck.Request{Msg: "正常消息内容你好世界", UserID: "42", Meta: spamcheck.MetaData{ChatID: -100, MessageID: i}, SuppressHamRecording: suppress})
			require.Equal(t, i == 3, spam)
			if i == 3 {
				require.True(t, checks[0].Spam)
				require.NotEmpty(t, checks[0].ExtraDeleteIDs)
			}
		}
		if suppress {
			require.Empty(t, d.hamHistory.Last(10))
			require.Empty(t, d.approvedUsers)
		} else {
			require.NotEmpty(t, d.hamHistory.Last(10))
			require.NotEmpty(t, d.approvedUsers)
		}
		require.Len(t, d.spamHistory.Last(10), 1)
	}
}

func TestCheckOnlyStillRecordsHamHistory(t *testing.T) {
	d := NewDetector(Config{FirstMessageOnly: true, MinMsgLen: 1, MaxAllowedEmoji: -1, HistorySize: 10})
	_, _ = d.Check(spamcheck.Request{Msg: "обычный нормальный текст", UserID: "42", CheckOnly: true})
	require.Empty(t, d.approvedUsers)
	require.Len(t, d.hamHistory.Last(10), 1, "CheckOnly is not a replacement for suppressing ham recording")
}
