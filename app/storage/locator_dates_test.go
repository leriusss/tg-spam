package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
)

func (s *StorageTestSuite) TestLocatorMessageDates() {
	for _, db := range s.getTestDB() {
		s.Run(string(db.Type), func() {
			ctx := context.Background()
			loc, err := NewLocator(ctx, time.Hour, 1000, db.DB)
			s.Require().NoError(err)
			date := time.Now().Add(-53 * time.Hour).Unix()
			s.Require().NoError(loc.AddMessage(ctx, "old original", 9876, 501, "", 8765))
			s.Require().NoError(loc.RecordMessageDate(ctx, 9876, 8765, date))
			dates, err := loc.MessageDates(ctx, 9876, 8765)
			s.Require().NoError(err)
			s.Equal(date, dates.MessageDate)
			s.Greater(dates.ObservedAt, date)
			s.Require().NoError(loc.RecordMessageDate(ctx, 9876, 8765, 0))
			again, err := loc.MessageDates(ctx, 9876, 8765)
			s.Require().NoError(err)
			s.Equal(dates, again, "an edit must preserve original date and first observation")
			unknown, err := loc.MessageDates(ctx, 9877, 8765)
			s.Require().NoError(err)
			s.Zero(unknown.MessageDate, "dates are scoped to the original chat")
		})
	}
}

func TestLocatorMessageDatesPersistAndLegacyRowsStayUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dates.db")
	db, err := engine.NewSqlite(path, "dates")
	require.NoError(t, err)
	loc, err := NewLocator(ctx, time.Hour, 1000, db)
	require.NoError(t, err)
	require.NoError(t, loc.AddMessage(ctx, "known", 123, 501, "", 1))
	require.NoError(t, loc.AddMessage(ctx, "legacy", 123, 501, "", 2))
	date := time.Now().Add(-time.Hour).Unix()
	require.NoError(t, loc.RecordMessageDate(ctx, 123, 1, date))
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "dates")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	loc, err = NewLocator(ctx, time.Hour, 1000, db)
	require.NoError(t, err)
	dates, err := loc.MessageDates(ctx, 123, 1)
	require.NoError(t, err)
	assert.Equal(t, date, dates.MessageDate)
	legacy, err := loc.MessageDates(ctx, 123, 2)
	require.NoError(t, err)
	assert.Zero(t, legacy.MessageDate)
	assert.Zero(t, legacy.ObservedAt)
	meta, found := loc.Message(ctx, "legacy")
	require.True(t, found)
	assert.False(t, meta.Time.IsZero(), "processing time is retained without inventing a send date")
}
