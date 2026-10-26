package moderation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func checkReadCardImmutable(t *testing.T, j *Journal, db *engine.SQL) {
	t.Helper()
	p := plan(881, "ban", "original_delete")
	p.Card, p.Learn, p.Origin = true, false, "detector"
	key := accept(t, j, p, time.Now())
	dump := func() string {
		var result strings.Builder
		for _, table := range []string{"moderation_control", "moderation_receipts", "moderation_actions", "moderation_demands", "moderation_decisions", "moderation_requests", "moderation_resolutions", "moderation_cards"} {
			rows, err := db.QueryxContext(context.Background(), "SELECT * FROM "+table)
			require.NoError(t, err)
			for rows.Next() {
				values, err := rows.SliceScan()
				require.NoError(t, err)
				result.WriteString(fmt.Sprint(values))
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
		}
		return result.String()
	}
	before := dump()
	for range 3 {
		c, err := j.ReadCardView(context.Background(), key)
		require.NoError(t, err)
		require.Positive(t, c.RecordedAt)
		require.NotEmpty(t, c.Audit)
	}
	require.Equal(t, before, dump(), "navigation cannot update even the journal control counter")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := j.ReadCardView(ctx, key)
	require.Error(t, err)
	require.Equal(t, before, dump())
}

func TestReadCardSQLiteImmutable(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	checkReadCardImmutable(t, j, db)
}

func TestReadCardPostgresImmutable(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL runs in full Linux CI")
	}
	ctx := context.Background()
	pg := containers.NewPostgresTestContainerWithDB(ctx, t, "card_read")
	defer pg.Close(ctx)
	db, err := engine.NewPostgres(ctx, pg.ConnectionString(), "card-read")
	require.NoError(t, err)
	defer db.Close()
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	checkReadCardImmutable(t, j, db)
}
