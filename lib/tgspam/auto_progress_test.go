package tgspam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func TestAutoProgressRestart(t *testing.T) {
	for count := range 4 {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.sqlite")
			open := func() (*Detector, func()) {
				db, err := engine.NewSqlite(path, "chat")
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				users, err := storage.NewApprovedUsers(context.Background(), db)
				require.NoError(t, err)
				d := progressModel(t, 3)
				_, err = d.WithUserStorage(users)
				require.NoError(t, err)
				return d, func() { require.NoError(t, db.Close()) }
			}
			d, closeDB := open()
			req := spamcheck.Request{UserID: "subject", Msg: "normal friendly conversation"}
			for range count {
				_, checks := d.Check(req)
				require.NotContains(t, responseNames(checks), "pre-approved")
			}
			closeDB()
			for range 3 {
				d, closeDB = open()
				require.Equal(t, count, d.approvedUsers[req.UserID].Count)
				req.CheckOnly = true
				_, checks := d.Check(req)
				require.Equal(t, count == 3, containsResponse(checks, "pre-approved"))
				closeDB()
			}
		})
	}
}

func autoProgressDB(t *testing.T) (*engine.SQL, *storage.ApprovedUsers) {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "users.sqlite"), "instance")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	users, err := storage.NewApprovedUsers(context.Background(), db)
	require.NoError(t, err)
	return db, users
}

func autoProgressDetector(t *testing.T, users UserStorage, threshold int) *Detector {
	t.Helper()
	d := progressModel(t, threshold)
	_, err := d.WithUserStorage(users)
	require.NoError(t, err)
	return d
}

func progressModel(t *testing.T, threshold int) *Detector {
	t.Helper()
	d := NewDetector(Config{FirstMessagesCount: threshold, MaxAllowedEmoji: -1, MinMsgLen: 5,
		SimilarityThreshold: 0.5, MinSpamProbability: 45})
	_, err := d.LoadSamples(strings.NewReader(""), []io.Reader{strings.NewReader("win lottery prize")},
		[]io.Reader{strings.NewReader("normal friendly conversation")})
	require.NoError(t, err)
	return d
}

func progressRequest(user string, chat int64, message int) spamcheck.Request {
	return spamcheck.Request{UserID: user, Msg: "normal friendly conversation",
		Meta: spamcheck.MetaData{ChatID: chat, MessageID: message}}
}

func TestAutoProgressTelegramRestartAndThreshold(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 3)
	for id := 1; id <= 3; id++ {
		_, checks := d.Check(progressRequest("user", -100, id))
		require.NotContains(t, responseNames(checks), "pre-approved")
		require.Contains(t, responseNames(checks), "classifier")
		require.Contains(t, responseNames(checks), "similarity")
		d = autoProgressDetector(t, users, 3)
		require.Equal(t, id, d.approvedUsers["user"].Progress[-100].Count)
		_, checks = d.Check(progressRequest("user", -100, id))
		require.Equal(t, id, d.approvedUsers["user"].Progress[-100].Count, "redelivery after restart")
		require.Equal(t, id == 3, containsResponse(checks, "pre-approved"))
	}
	_, checks := d.Check(progressRequest("user", -100, 4))
	require.Contains(t, responseNames(checks), "pre-approved")
	d = autoProgressDetector(t, users, 4)
	_, checks = d.Check(progressRequest("user", -100, 4))
	require.NotContains(t, responseNames(checks), "pre-approved")
	require.Equal(t, 4, d.approvedUsers["user"].Progress[-100].Count)
	d = autoProgressDetector(t, users, 2)
	_, checks = d.Check(progressRequest("user", -100, 5))
	require.Contains(t, responseNames(checks), "pre-approved")
	require.NoError(t, d.RemoveApprovedUser("user"))
	d = autoProgressDetector(t, users, 3)
	_, checks = d.Check(progressRequest("user", -100, 6))
	require.NotContains(t, responseNames(checks), "pre-approved")
	require.Equal(t, 1, d.approvedUsers["user"].Progress[-100].Count)
}

func TestAutoProgressLegacyAndManual(t *testing.T) {
	db, users := autoProgressDB(t)
	ctx := context.Background()
	for _, source := range []string{approved.SourceAuto, approved.SourceManual, approved.SourceUnknown, "", "import-v1"} {
		_, err := db.Exec("INSERT INTO approved_users (uid,gid,name,source) VALUES (?,?,?,?)", source+"user", db.GID(), "fixture", source)
		require.NoError(t, err)
	}
	d := autoProgressDetector(t, users, 3)
	for _, source := range []string{approved.SourceAuto, approved.SourceManual, approved.SourceUnknown, "", "import-v1"} {
		user := source + "user"
		_, checks := d.Check(progressRequest(user, -100, 1))
		require.Equal(t, source == approved.SourceManual, containsResponse(checks, "pre-approved"))
		require.Equal(t, source == approved.SourceManual, d.IsExplicitTrustedUser(user))
		var actual string
		require.NoError(t, db.GetContext(ctx, &actual, "SELECT source FROM approved_users WHERE uid=?", user))
		// empty legacy provenance is preserved as well as non-standard source strings.
		require.Equal(t, source, actual)
	}
	d = autoProgressDetector(t, users, 100)
	_, checks := d.Check(progressRequest("manualuser", -100, 2))
	require.Contains(t, responseNames(checks), "pre-approved")
}

type failingProgressStorage struct {
	UserStorage
	fail bool
}

func (s *failingProgressStorage) Write(ctx context.Context, user approved.UserInfo) error {
	if s.fail {
		return errors.New("storage offline")
	}
	return s.UserStorage.Write(ctx, user)
}

func TestAutoProgressSaveFailure(t *testing.T) {
	_, users := autoProgressDB(t)
	store := &failingProgressStorage{UserStorage: users, fail: true}
	d := autoProgressDetector(t, store, 3)
	for id := 1; id <= 4; id++ {
		spam, checks := d.Check(progressRequest("user", -100, id))
		require.False(t, spam)
		require.ErrorContains(t, checks[len(checks)-1].Error, "storage offline")
		require.Empty(t, d.approvedUsers)
	}
	rows, err := users.Read(context.Background())
	require.NoError(t, err)
	require.Empty(t, rows)
	store.fail = false
	d.Check(progressRequest("user", -100, 1))
	d = autoProgressDetector(t, users, 3)
	require.Equal(t, 1, d.approvedUsers["user"].Progress[-100].Count)
	_, checks := d.Check(progressRequest("user", -100, 1))
	require.NotContains(t, responseNames(checks), "pre-approved")
	require.Equal(t, 1, d.approvedUsers["user"].Progress[-100].Count)
}

func TestAutoProgressExcludedChecks(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 3)
	req := progressRequest("user", -100, 1)
	req.CheckOnly = true
	d.Check(req)
	req.CheckOnly = false
	req.Msg = "hi"
	d.Check(req)
	d.WithMetaChecks(func(spamcheck.Request) spamcheck.Response { return spamcheck.Response{Name: "mock", Spam: true} })
	req.Msg = "normal friendly conversation"
	spam, _ := d.Check(req)
	require.True(t, spam)
	require.Empty(t, d.approvedUsers)
	d.metaChecks = nil
	for _, broken := range []spamcheck.MetaData{{ChatID: -100}, {MessageID: 1}} {
		req.Meta = broken
		_, checks := d.Check(req)
		require.Equal(t, "auto approval persistence", checks[len(checks)-1].Name)
	}
	require.Empty(t, d.approvedUsers)
}

func TestAutoProgressConcurrentIdentity(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 1000)
	var wg sync.WaitGroup
	for _, user := range []string{"123", "456", "-123"} {
		for _, chat := range []int64{-100, -200} {
			for id := 1; id <= 20; id++ {
				for range 2 {
					wg.Go(func() { d.Check(progressRequest(user, chat, id)) })
				}
			}
		}
	}
	wg.Wait()
	d = autoProgressDetector(t, users, 1000)
	for _, user := range []string{"123", "456", "-123"} {
		for _, chat := range []int64{-100, -200} {
			require.Equal(t, 20, d.approvedUsers[user].Progress[chat].Count, fmt.Sprint(user, chat))
		}
	}
	// mutating an exposed snapshot must not race with, or change, detector state.
	snapshot := d.ApprovedUsers()
	snapshot[0].Progress[-100] = approved.CheckProgress{Count: 9999}
	require.Equal(t, 20, d.approvedUsers[snapshot[0].UserID].Progress[-100].Count)
}

func TestAutoProgressNoGlobalWriteLockDuringChecks(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 3)
	entered, release, fast := make(chan struct{}), make(chan struct{}), make(chan struct{})
	d.WithMetaChecks(func(req spamcheck.Request) spamcheck.Response {
		if req.UserID == "slow" {
			close(entered)
			<-release
		} else {
			close(fast)
		}
		return spamcheck.Response{Name: "external mock"}
	})
	done := make(chan struct{})
	go func() { d.Check(progressRequest("slow", -100, 1)); close(done) }()
	<-entered
	// a separate user enters external checks while the slow request is still blocked.
	fastDone := make(chan struct{})
	go func() { d.Check(progressRequest("fast", -100, 1)); close(fastDone) }()
	<-fast
	close(release)
	<-done
	<-fastDone
	require.Equal(t, 1, d.approvedUsers["fast"].Progress[-100].Count)
}

func responseNames(checks []spamcheck.Response) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name)
	}
	return names
}

func containsResponse(checks []spamcheck.Response, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func TestAutoProgressScopedApprovalAPI(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 3)
	for id := 1; id <= 3; id++ {
		d.Check(progressRequest("subject", -100, id))
		require.Equal(t, id == 3, d.IsApprovedUserInChat("subject", -100))
		require.Equal(t, id == 3, d.IsApprovedUser("subject"), "legacy API remains available at the same threshold")
		require.False(t, d.IsApprovedUserInChat("subject", -200))
		require.False(t, d.IsApprovedUserInChat("subject", 0))
	}
	d = autoProgressDetector(t, users, 3)
	require.True(t, d.IsApprovedUserInChat("subject", -100))
	require.False(t, d.IsApprovedUserInChat("subject", -200))
	require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "manual"}))
	require.True(t, d.IsApprovedUserInChat("manual", -200))
	d.RemoveApprovedUser("subject")
	require.False(t, d.IsApprovedUserInChat("subject", -100))
	d = autoProgressDetector(t, users, 3)
	require.False(t, d.IsApprovedUser("subject"))
	d.Check(progressRequest("subject", -100, 1))
	require.Equal(t, 1, d.approvedUsers["subject"].Progress[-100].Count, "removal also resets the replay window")
}

func TestAutoProgressConcurrentThresholdWaitsForCompletedChecks(t *testing.T) {
	_, users := autoProgressDB(t)
	d := autoProgressDetector(t, users, 3)
	d.Check(progressRequest("subject", -100, 1))
	d.Check(progressRequest("subject", -100, 2))
	entered, another, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	d.WithMetaChecks(func(req spamcheck.Request) spamcheck.Response {
		if req.Meta.MessageID == 3 {
			close(entered)
			<-release
		}
		if req.Meta.MessageID == 4 {
			close(another)
		}
		return spamcheck.Response{Name: "in-flight check"}
	})
	done := make(chan []spamcheck.Response, 2)
	go func() { _, checks := d.Check(progressRequest("subject", -100, 3)); done <- checks }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("third check did not start")
	}
	go func() { _, checks := d.Check(progressRequest("subject", -100, 4)); done <- checks }()
	select {
	case <-another:
	case <-time.After(5 * time.Second):
		t.Fatal("unfinished third check must not authorize bypass")
	}
	once.Do(func() { close(release) })
	for range 2 {
		checks := <-done
		require.Contains(t, responseNames(checks), "classifier")
		require.NotContains(t, responseNames(checks), "pre-approved")
	}
	d = autoProgressDetector(t, users, 3)
	require.Equal(t, 4, d.approvedUsers["subject"].Progress[-100].Count, "both completed checks persist without lost increments")
	_, checks := d.Check(progressRequest("subject", -100, 5))
	require.Contains(t, responseNames(checks), "pre-approved")
	require.Equal(t, 4, d.approvedUsers["subject"].Progress[-100].Count, "bypass adds no synthetic increment")
}
