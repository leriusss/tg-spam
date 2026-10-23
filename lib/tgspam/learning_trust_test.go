package tgspam

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func TestLearningUnavailableDoesNotBuildTrust(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "users.sqlite")
	db, err := engine.NewSqlite(path, "trust-test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	users, err := storage.NewApprovedUsers(ctx, db)
	require.NoError(t, err)
	store := &learningStore{rows: map[string]spamClass{
		"hello peaceful world": ClassHam, "normal friendly conversation": ClassHam, "win lottery prize": ClassSpam,
	}}
	makeDetector := func() *Detector {
		d := learningDetector(t, store)
		d.FirstMessagesCount = 3
		d.FirstMessageOnly = true
		d.hamHistory = spamcheck.NewLastRequests(20)
		_, loadErr := d.WithUserStorage(users)
		require.NoError(t, loadErr)
		return d
	}
	d := makeDetector()
	require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "manual", UserName: "operator"}))
	baseline, err := users.Read(ctx)
	require.NoError(t, err)
	store.readErr = errors.New("snapshot unavailable")
	require.ErrorContains(t, d.UpdateSpam("additional lottery prize"), "sample saved but model unavailable")
	require.Equal(t, ClassSpam, store.rows["additional lottery prize"])
	req := spamcheck.Request{Msg: "hello peaceful world", UserID: "subject", CheckOnly: false}
	for range 5 {
		spam, checks := d.Check(req)
		require.False(t, spam)
		require.Equal(t, "sample model unavailable", checks[0].Name)
		require.Error(t, checks[0].Error)
		require.Zero(t, d.approvedUsers[req.UserID].Count)
		require.Empty(t, d.hamHistory.Last(20))
		persisted, readErr := users.Read(ctx)
		require.NoError(t, readErr)
		require.Equal(t, baseline, persisted)
	}
	d.WithMetaChecks(func(spamcheck.Request) spamcheck.Response {
		return spamcheck.Response{Name: "independent", Spam: true}
	})
	spam, checks := d.Check(req)
	require.True(t, spam)
	require.Error(t, checks[0].Error)
	require.True(t, checks[1].Spam)
	require.Empty(t, d.hamHistory.Last(20))
	require.Zero(t, d.approvedUsers[req.UserID].Count)
	d.metaChecks = nil
	store.readErr = nil
	spam, checks = d.Check(req)
	require.False(t, spam)
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
		require.NoError(t, check.Error)
	}
	require.Contains(t, names, "classifier")
	require.Contains(t, names, "similarity")
	require.NotContains(t, names, "pre-approved")
	require.Equal(t, 1, d.approvedUsers[req.UserID].Count)
	require.Len(t, d.hamHistory.Last(20), 1)

	// only completed checks survive a restart; one check does not reach the threshold.
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "trust-test")
	require.NoError(t, err)
	users, err = storage.NewApprovedUsers(ctx, db)
	require.NoError(t, err)
	d = makeDetector()
	require.Equal(t, approved.SourceManual, d.approvedUsers["manual"].Source)
	require.Equal(t, approved.SourceAuto, d.approvedUsers["subject"].Source)
	_, checks = d.Check(req)
	require.NotContains(t, responseNames(checks), "pre-approved")
	require.Contains(t, responseNames(checks), "classifier")
	require.Contains(t, responseNames(checks), "similarity")
	req.UserID = "outage-only"
	d.hamHistory = spamcheck.NewLastRequests(20)
	store.readErr = errors.New("snapshot unavailable")
	require.Error(t, d.UpdateSpam("additional lottery prize"))
	for range 5 {
		d.Check(req)
	}
	require.Zero(t, d.approvedUsers[req.UserID].Count)
	require.Empty(t, d.hamHistory.Last(20))
	store.readErr = nil
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "trust-test")
	require.NoError(t, err)
	users, err = storage.NewApprovedUsers(ctx, db)
	require.NoError(t, err)
	d = makeDetector()
	require.Zero(t, d.approvedUsers[req.UserID].Count)
	_, checks = d.Check(req)
	names = nil
	for _, check := range checks {
		names = append(names, check.Name)
	}
	require.Contains(t, names, "classifier")
	require.Contains(t, names, "similarity")
	require.NotContains(t, names, "pre-approved")
	require.Equal(t, 1, d.approvedUsers[req.UserID].Count)
}
