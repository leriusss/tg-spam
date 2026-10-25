package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
)

// Use a subprocess because execute owns process-lifetime database/storage
// resources. All Telegram traffic remains on the child's loopback HTTP server.
func TestTelegramStartupExecute(t *testing.T) {
	if scenario := os.Getenv("TGSPAM_STARTUP_TEST_CHILD"); scenario != "" {
		testTelegramStartupExecuteChild(t, scenario)
		return
	}
	for _, scenario := range []string{"retry then success", "permanent failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTelegramStartupExecute$", "-test.v")
			cmd.Env = append(os.Environ(), "TGSPAM_STARTUP_TEST_CHILD="+scenario, "TGSPAM_STARTUP_TEST_DIR="+t.TempDir())
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Contains(t, string(output), "PASS")
			require.NotContains(t, string(output), fakeTelegramToken)
		})
	}
}

func testTelegramStartupExecuteChild(t *testing.T, scenario string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := os.Getenv("TGSPAM_STARTUP_TEST_DIR")
	var opts options
	_, err := flags.NewParser(&opts, flags.Default).ParseArgs(nil)
	require.NoError(t, err)
	opts.Telegram.Token, opts.Telegram.Group = fakeTelegramToken, "-100"
	opts.AdminGroup, opts.InstanceID = "-200", "startup-fixture"
	opts.Files.DynamicDataPath, opts.Files.SamplesDataPath = dir, dir
	opts.DataBaseURL = filepath.Join(dir, "startup.db")
	opts.MaxBackups, opts.ModerationRecovery = 0, true
	opts.Convert, opts.Message.Startup = "disabled", ""
	opts.Server.Enabled, opts.Logger.Enabled, opts.TGDbg = false, false, false
	db, err := engine.NewSqlite(opts.DataBaseURL, opts.InstanceID)
	require.NoError(t, err)
	defer db.Close()
	samples, err := storage.NewSamples(ctx, db)
	require.NoError(t, err)
	require.NoError(t, samples.Add(ctx, storage.SampleTypeHam, storage.SampleOriginPreset, "normal friendly conversation"))
	require.NoError(t, samples.Add(ctx, storage.SampleTypeSpam, storage.SampleOriginPreset, "win lottery prize now"))
	journal, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
	require.NoError(t, err)
	_, _, _, err = journal.Accept(ctx, moderation.Plan{Key: "source:-100:1:user:7", ChatID: -100, IdentityID: 7,
		MessageID: 1, Destination: -200, IdentityKind: "user", Actions: []moderation.Action{{Kind: "original_delete",
			ChatID: -100, IdentityID: 7, IdentityKind: "user", MessageID: 1, MessageDate: time.Now().Unix()}}}, time.Now())
	require.NoError(t, err)
	var getMe, polling, notices atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch method {
		case "getMe":
			n := getMe.Add(1)
			if scenario == "permanent failure" || n == 1 {
				code := 503
				if scenario == "permanent failure" {
					code = 401
				}
				w.WriteHeader(code)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"fixture"}`, code)
				return
			}
			_, _ = io.WriteString(w, startupUserJSON)
		case "getChatAdministrators":
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "getChat":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":-100,"type":"supergroup","title":"fixture"}}`)
		case "getUpdates":
			polling.Add(1)
			<-r.Context().Done()
		case "getChatMember":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"status":"member","user":{"id":7,"is_bot":false,"first_name":"fixture"}}}`)
		case "deleteMessage":
			notices.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":999}}`)
		default:
			t.Errorf("unexpected runtime method %s", method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	var bot atomic.Pointer[tbapi.BotAPI]
	// Cancel the library's polling context even when an assertion fails.
	defer func() {
		if api := bot.Load(); api != nil {
			api.StopReceivingUpdates()
		}
		cancel()
	}()
	clock := newStartupTestClock()
	policy := clock.policy()
	wait := policy.wait
	policy.wait = func(waitCtx context.Context, delay time.Duration) error {
		require.Zero(t, polling.Load(), "polling before successful getMe")
		require.Zero(t, notices.Load(), "recovery worker before successful getMe")
		require.Zero(t, startupWorkerGoroutines(t))
		rows, e := journal.Snapshot(ctx)
		require.NoError(t, e)
		for _, action := range rows {
			require.Zero(t, action.Attempts)
		}
		return wait(waitCtx, delay)
	}
	done := make(chan error, 1)
	go func() {
		done <- executeWithTelegram(ctx, opts,
			func(startCtx context.Context, token, _ string, client *telegramHTTPClient) (*tbapi.BotAPI, error) {
				client.client = server.Client()
				api, e := policy.initialize(startCtx, token, server.URL+"/bot%s/%s", client)
				if api != nil {
					bot.Store(api)
				}
				return api, e
			})
	}()
	if scenario == "permanent failure" {
		select {
		case err = <-done:
			require.ErrorContains(t, err, "http_401")
		case <-time.After(5 * time.Second):
			t.Fatal("initialization did not stop")
		}
		require.EqualValues(t, 1, getMe.Load())
		require.Zero(t, polling.Load())
		require.Zero(t, notices.Load())
		rows, e := journal.Snapshot(ctx)
		require.NoError(t, e)
		for _, action := range rows {
			require.Zero(t, action.Attempts)
		}
		return
	}
	var earlyErr error
	require.Eventually(t, func() bool {
		select {
		case earlyErr = <-done:
			return true
		default:
		}
		if polling.Load() != 1 {
			return false // Do not compete with startup schema initialization.
		}
		rows, e := journal.Snapshot(ctx)
		if e != nil {
			return false
		}
		for _, action := range rows {
			if action.Kind == "original_delete" {
				return polling.Load() == 1 && action.State == moderation.Confirmed && action.Attempts == 1
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, earlyErr, "startup failed before polling and worker became ready")
	require.EqualValues(t, 2, getMe.Load())
	require.EqualValues(t, 1, notices.Load())
	require.EqualValues(t, 1, polling.Load())
	require.Equal(t, 1, startupWorkerGoroutines(t), "one recovery worker after initialization")
	bot.Load().StopReceivingUpdates()
	cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, startupWorkerGoroutines(t), "worker must be joined on listener cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("listener/worker did not stop")
	}
}

func startupWorkerGoroutines(t *testing.T) int {
	t.Helper()
	var stacks bytes.Buffer
	require.NoError(t, pprof.Lookup("goroutine").WriteTo(&stacks, 2))
	return strings.Count(stacks.String(), "github.com/umputun/tg-spam/app/moderation.(*Worker).Run(")
}
