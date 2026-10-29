package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/events"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

// The real client verifies api.telegram.org against a fixture CA, both directly
// and inside an authenticated TLS CONNECT tunnel. No external traffic is sent.
type routeFixture struct {
	r           *telegramRoutes
	c           *telegramHTTPClient
	directFail  atomic.Bool
	reserveFail atomic.Bool
	authFail    atomic.Bool
	status      atomic.Int64
	connects    atomic.Int64
	directDials atomic.Int64
	calls       atomic.Int64
	methodsMu   sync.Mutex
	methods     []string
	handler     func(http.ResponseWriter, *http.Request)
}

func routeCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "route-fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"api.telegram.org"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	return cert, pool
}

func newRouteFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *routeFixture {
	t.Helper()
	f := &routeFixture{handler: handler}
	cert, pool := routeCertificate(t)
	api := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "api.telegram.org", req.TLS.ServerName)
		assert.Empty(t, req.Header.Get("Proxy-Authorization"))
		f.calls.Add(1)
		f.methodsMu.Lock()
		f.methods = append(f.methods, req.URL.Path)
		f.methodsMu.Unlock()
		if f.handler != nil {
			f.handler(w, req)
			return
		}
		if status := int(f.status.Load()); status != 0 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"ok":false,"parameters":{"retry_after":600}}`)
			return
		}
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		} else {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
		}
	}))
	api.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	api.StartTLS()
	t.Cleanup(api.Close)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.authFail.Load() || req.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNzd29yZA==" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if f.reserveFail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		assert.Equal(t, http.MethodConnect, req.Method)
		assert.Equal(t, "api.telegram.org:443", req.Host)
		upstream, err := net.DialTimeout("tcp", api.Listener.Addr().String(), time.Second)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if !assert.NoError(t, err) {
			_ = upstream.Close()
			return
		}
		f.connects.Add(1)
		_, _ = downstream.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { defer upstream.Close(); defer downstream.Close(); _, _ = io.Copy(upstream, downstream) }()
		_, _ = io.Copy(downstream, upstream)
		_ = upstream.Close()
		_ = downstream.Close()
	}))
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	proxyURL.User = url.UserPassword("user", "password")
	transports := [2]*http.Transport{}
	for idx := range transports {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		transport.TLSHandshakeTimeout = 100 * time.Millisecond
		if idx == telegramDirect {
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				f.directDials.Add(1)
				if f.directFail.Load() {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", api.Listener.Addr().String())
			}
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
			transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
				if resp.StatusCode == http.StatusProxyAuthRequired {
					return telegramProxyAuthError{}
				}
				return nil
			}
		}
		transports[idx] = transport
	}
	f.r = newTelegramRoutes(context.Background(), "777:fixture", transports)
	f.r.logf = t.Logf
	f.c = &telegramHTTPClient{client: f.r, routeHTTP: true, recoveryHTTP: true}
	t.Cleanup(func() {
		f.r.cancel()
		for _, tr := range transports {
			tr.CloseIdleConnections()
		}
	})
	return f
}

func (f *routeFixture) request(method string) error {
	req, _ := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot777:fixture/"+method, http.NoBody)
	resp, err := f.c.Do(req)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return err
}

func TestTelegramRoutesFailoverAndReturn(t *testing.T) {
	f := newRouteFixture(t, nil)
	require.NoError(t, f.request("getUpdates"))
	f.r.checkRoutes()
	assert.Zero(t, f.connects.Load(), "healthy direct path does not use the proxy")
	assert.False(t, f.r.snapshot().LastPolling.IsZero(), "empty successful getUpdates is healthy")
	f.r.transports[0].CloseIdleConnections()
	f.directFail.Store(true)
	require.Error(t, f.request("getUpdates"))
	f.r.checkRoutes()
	assert.Equal(t, "direct", f.r.snapshot().Selected)
	for range 2 {
		require.Error(t, f.request("getUpdates"))
	}
	start := time.Now()
	f.r.checkRoutes()
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
	assert.Positive(t, f.connects.Load(), "CONNECT actually reached an independent fixture listener")
	require.NoError(t, f.request("getUpdates"))
	t.Logf("fixture recovery after third failed request: %s", time.Since(start))
	base := time.Now()
	f.r.now = func() time.Time { return base }
	f.directFail.Store(false)
	for range 2 {
		base = base.Add(40 * time.Second)
		f.r.checkRoutes()
	}
	assert.Equal(t, "reserve", f.r.snapshot().Selected, "minimum dwell applies")
	base = base.Add(40 * time.Second)
	f.r.checkRoutes()
	assert.Equal(t, "direct", f.r.snapshot().Selected)
	assert.Equal(t, "primary_recovered", f.r.snapshot().SwitchReason)
	assert.Equal(t, int64(1), f.connects.Load(), "per-route pools reuse the CONNECT tunnel")
}

func TestTelegramRoutesBrokenReserveBackoff(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.directFail.Store(true)
	f.reserveFail.Store(true)
	for range 3 {
		require.Error(t, f.request("getUpdates"))
	}
	f.r.checkRoutes()
	assert.Equal(t, "unavailable", f.r.snapshot().State)
	assert.Equal(t, "direct", f.r.snapshot().Selected)
	assert.NotEqual(t, "ok", f.r.snapshot().Probes[1])
	before := f.r.nextProbe
	for range 100 {
		f.r.checkRoutes()
	}
	assert.Equal(t, before, f.r.nextProbe)
	assert.GreaterOrEqual(t, f.r.probeDelay, 60*time.Second)
}

func TestTelegramRoutesHTTPRefusalsAndCooldown(t *testing.T) {
	for _, status := range []int{401, 403, 409, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newRouteFixture(t, nil)
			f.status.Store(int64(status))
			var paused time.Time
			f.c.pause = func(_ context.Context, until time.Time) error { paused = until; return nil }
			for range 4 {
				_ = f.request("getUpdates")
			}
			f.r.checkRoutes()
			assert.Equal(t, "direct", f.r.snapshot().Selected)
			assert.Zero(t, f.connects.Load())
			if status == 429 {
				assert.Greater(t, time.Until(paused), 599*time.Second)
			}
		})
	}
	f := newRouteFixture(t, nil)
	f.r.cooldown = func(context.Context) (time.Time, error) { return time.Now().Add(time.Hour), nil }
	ctx, cancel := context.WithCancel(f.r.ctx)
	cancel()
	f.r.ctx = ctx
	assert.False(t, f.r.probe(1))
	assert.Zero(t, f.connects.Load(), "probes cannot bypass durable cooldown")
}

func TestTelegramRoutesPollingNotMaskedByGetMe(t *testing.T) {
	f := newRouteFixture(t, nil)
	request, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot777:fixture/getUpdates", http.NoBody)
	require.NoError(t, err)
	for range 3 {
		f.r.observe(0, request, false, io.ErrUnexpectedEOF, "response")
		require.NoError(t, f.request("getMe"))
	}
	s := f.r.snapshot()
	assert.Equal(t, 3, s.PollingFailures[0])
	assert.True(t, s.LastPolling.IsZero())
	assert.False(t, s.LastExchange.IsZero())
	f.r.checkRoutes()
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
}

func TestTelegramRoutesMutationFailureNotMaskedByTargetLookups(t *testing.T) {
	f := newRouteFixture(t, nil)
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot777:fixture/deleteMessage", http.NoBody)
	require.NoError(t, err)
	for range 3 {
		f.r.observe(0, req, false, io.ErrUnexpectedEOF, "response")
		require.NoError(t, f.request("getChatMember"))
		require.NoError(t, f.request("getMe"))
	}
	assert.Equal(t, 3, f.r.snapshot().WorkerFailures[0])
	f.r.checkRoutes()
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
}

func TestTelegramRoutesCooldownWithoutJournal(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.status.Store(429)
	f.c.recoveryHTTP = false
	limiter := &telegramRouteCooldown{}
	f.c.cooldown, f.c.pause = limiter.load, limiter.store
	err := f.request("getMe")
	require.Error(t, err)
	until, err := limiter.load(context.Background())
	require.NoError(t, err)
	assert.Greater(t, time.Until(until), 599*time.Second)
	f.r.cooldown, f.r.pause = limiter.load, limiter.store
	ctx, cancel := context.WithCancel(f.r.ctx)
	cancel()
	f.r.ctx = ctx
	assert.False(t, f.r.probe(1))
	assert.Zero(t, f.connects.Load())
}

func TestTelegramRoutesProbe429CannotBeBypassed(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.status.Store(429)
	limiter := &telegramRouteCooldown{}
	f.r.cooldown, f.r.pause = limiter.load, limiter.store
	assert.False(t, f.r.probe(1))
	assert.Equal(t, "http_429", f.r.snapshot().Probes[1])
	calls := f.calls.Load()
	assert.False(t, f.r.probe(0))
	assert.Equal(t, "cooldown", f.r.snapshot().Probes[0])
	assert.Equal(t, calls, f.calls.Load(), "the other route must not bypass the full probe Retry-After")
	until, err := limiter.load(context.Background())
	require.NoError(t, err)
	assert.Greater(t, time.Until(until), 599*time.Second)
}

func TestTelegramRoutesEnabledConfigurationOwnsPoolsAndLifecycle(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://untrusted.invalid:1234")
	file := filepath.Join(t.TempDir(), "proxy-url")
	require.NoError(t, os.WriteFile(file, []byte("https://user:password@127.0.0.1:9"), 0o600))
	client := newTelegramHTTPClient()
	closeRoutes, err := configureTelegramRoutes(context.Background(), client, "777:fixture", file)
	require.NoError(t, err)
	routes := client.client.(*telegramRoutes)
	assert.Nil(t, routes.transports[0].Proxy)
	assert.NotSame(t, routes.transports[0], routes.transports[1])
	assert.NotNil(t, client.cooldown)
	assert.NotNil(t, client.pause)
	closeRoutes()
	select {
	case <-routes.done:
	default:
		t.Fatal("controller not joined")
	}
}

func TestTelegramRoutesCertificatesAuthAndSecrets(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.r.transports[0].TLSClientConfig.RootCAs = x509.NewCertPool()
	err := f.request("getMe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
	assert.NotContains(t, err.Error(), "777:fixture")
	assert.Zero(t, f.r.snapshot().Failures[0])
	f.authFail.Store(true)
	assert.False(t, f.r.probe(1))
	assert.Equal(t, "proxy_auth", f.r.snapshot().Probes[1])
	f.r.mu.Lock()
	f.r.active = 1
	f.r.mu.Unlock()
	err = f.request("getMe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy_auth")
	assert.NotContains(t, err.Error(), "password")
	assert.NotContains(t, err.Error(), "777:fixture")
	var auth telegramProxyAuthError
	assert.ErrorAs(t, err, &auth)
}

func TestTelegramRoutesClassificationAndCancellation(t *testing.T) {
	for _, item := range []struct {
		err   error
		class string
	}{
		{&net.DNSError{Err: "fixture", Name: "secret.invalid"}, "dns"},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, "tcp"},
		{io.ErrUnexpectedEOF, "read"}, {context.DeadlineExceeded, "timeout"},
		{context.Canceled, "canceled"}, {x509.UnknownAuthorityError{}, "certificate"},
	} {
		assert.Equal(t, item.class, telegramNetworkClass(item.err))
	}
	f := newRouteFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot777:fixture/getUpdates", http.NoBody)
	require.NoError(t, err)
	_, err = f.c.Do(req)
	require.Error(t, err)
	assert.Zero(t, f.r.snapshot().PollingFailures[0])
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	req = req.Clone(ctx)
	_, err = f.c.Do(req)
	require.Error(t, err)
	assert.Zero(t, f.r.snapshot().PollingFailures[0])
}

func TestTelegramRoutesConfigurationDefaultAndValidation(t *testing.T) {
	client := newTelegramHTTPClient()
	original := client.client
	closeRoutes, err := configureTelegramRoutes(context.Background(), client, "777:fixture", "")
	require.NoError(t, err)
	closeRoutes()
	assert.Same(t, original, client.client)
	for _, content := range []string{"bad%proxy:password", "http://user:password@proxy.invalid:443", "https://proxy.invalid", "https://user:password@proxy.invalid/path"} {
		file := filepath.Join(t.TempDir(), "proxy-url")
		require.NoError(t, os.WriteFile(file, []byte(content), 0o600))
		_, err = configureTelegramRoutes(context.Background(), client, "777:fixture", file)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "password")
		assert.NotContains(t, err.Error(), "proxy.invalid")
	}
}

func TestTelegramRoutesNoReplayOrSecondPolling(t *testing.T) {
	var offsets []string
	var mu sync.Mutex
	f := newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			assert.NoError(t, req.ParseForm())
			mu.Lock()
			offsets = append(offsets, req.Form.Get("offset"))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":41,"message":{"message_id":1,"date":1,"chat":{"id":-1,"type":"supergroup"}}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
	})
	bot, err := tbapi.NewBotAPIWithClient("777:fixture", tbapi.APIEndpoint, f.c)
	require.NoError(t, err)
	updates, err := bot.GetUpdates(tbapi.NewUpdate(0))
	require.NoError(t, err)
	require.Len(t, updates, 1)
	next := updates[0].UpdateID + 1
	f.r.transports[0].CloseIdleConnections()
	f.directFail.Store(true)
	for range 3 {
		_, err = bot.GetUpdates(tbapi.NewUpdate(next))
		require.Error(t, err)
	}
	f.r.checkRoutes()
	_, err = bot.GetUpdates(tbapi.NewUpdate(next))
	require.NoError(t, err)
	mu.Lock()
	assert.Equal(t, []string{"", "42"}, offsets)
	mu.Unlock()
	f.methodsMu.Lock()
	defer f.methodsMu.Unlock()
	assert.Equal(t, 2, strings.Count(strings.Join(f.methods, "\n"), "/getUpdates"), "probes only use getMe")
}

func TestTelegramRoutesNoHiddenMutatingRetry(t *testing.T) {
	var deletes atomic.Int64
	f := newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/deleteMessage") {
			deletes.Add(1)
			w.Header().Set("Content-Length", "200")
			_, _ = io.WriteString(w, `{"ok":true`)
			return // Ambiguous partial response after the server accepted the action.
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
	})
	err := f.request("deleteMessage")
	require.Error(t, err)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	f.r.mu.Lock()
	f.r.status.Failures[0] = 3
	f.r.mu.Unlock()
	f.r.checkRoutes()
	assert.Equal(t, int64(1), deletes.Load(), "route selection does not resubmit accepted ambiguous actions")
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
}

func TestTelegramRoutesProbeControllerStops(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.r.interval = time.Millisecond
	f.r.probeDelay = time.Millisecond
	f.r.logf = func(string, ...any) {}
	go f.r.run()
	for range 100 {
		require.NoError(t, f.request("getMe"))
	}
	f.r.close()
	assert.Zero(t, f.connects.Load())
}

// Keep type identity through safe errors for the existing startup/recovery classifiers.
func TestTelegramRoutesSafeErrorPreservesCause(t *testing.T) {
	unsafe := &url.Error{URL: "https://user:password@proxy.invalid/bot777:fixture/deleteMessage", Err: io.ErrUnexpectedEOF}
	safe := sanitizeTelegramHTTPError(telegramRouteError{cause: unsafe, class: "read"})
	require.ErrorIs(t, safe, io.ErrUnexpectedEOF)
	assert.Equal(t, "telegram route: read", safe.Error())
	assert.ErrorIs(t, safe, io.ErrUnexpectedEOF)
}

func TestTelegramRoutesStartupUsesSameController(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.directFail.Store(true)
	attempts := 0
	startup := telegramStartup{now: time.Now, logf: t.Logf, wait: func(_ context.Context, _ time.Duration) error {
		attempts++
		f.r.checkRoutes()
		return nil
	}}
	api, err := startup.initialize(context.Background(), "777:fixture", tbapi.APIEndpoint, f.c)
	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
	assert.Same(t, f.c, api.Client)
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
	assert.Equal(t, int64(2), f.calls.Load(), "one reserve getMe probe and the fourth constructor attempt")
	assert.Equal(t, int64(3), f.directDials.Load(), "no hidden retries within startup attempts")
}

func TestTelegramRoutesStartupLocalNXDOMAINCanUseIndependentProxy(t *testing.T) {
	f := newRouteFixture(t, nil)
	f.r.transports[0].DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "api.telegram.org", IsNotFound: true}
	}
	startup := telegramStartup{now: time.Now, logf: t.Logf, wait: func(context.Context, time.Duration) error {
		f.r.checkRoutes()
		return nil
	}}
	api, err := startup.initialize(context.Background(), "777:fixture", tbapi.APIEndpoint, f.c)
	require.NoError(t, err)
	assert.Same(t, f.c, api.Client)
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
	assert.Equal(t, int64(2), f.calls.Load())
}

func TestTelegramRoutesActiveRequestDoesNotPinNewRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f := newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			close(started)
			select {
			case <-req.Context().Done():
			case <-release:
			}
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot777:fixture/getUpdates", http.NoBody)
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() {
		resp, e := f.c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		finished <- e
	}()
	<-started
	f.r.transports[0].CloseIdleConnections()
	f.directFail.Store(true)
	for range 3 {
		require.Error(t, f.request("getMe"))
	}
	f.r.checkRoutes()
	require.NoError(t, f.request("getMe"))
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
	select {
	case err = <-finished:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("old active request outlived its deadline")
	}
}

func TestTelegramRoutesRealTLSAndReadTimeouts(t *testing.T) {
	t.Run("TLS handshake", func(t *testing.T) {
		f := newRouteFixture(t, nil)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, e := listener.Accept()
			if e == nil {
				accepted <- conn
			}
		}()
		f.r.transports[0].DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		}
		err = f.request("getUpdates")
		require.Error(t, err)
		conn := <-accepted
		_ = conn.Close()
		assert.Equal(t, "timeout", f.r.snapshot().LastError)
		assert.Equal(t, "tls", f.r.snapshot().LastStage)
		assert.Equal(t, 1, f.r.snapshot().PollingFailures[0])
	})
	t.Run("response read", func(t *testing.T) {
		f := newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":`)
			w.(http.Flusher).Flush()
			<-req.Context().Done()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		// Model the client's internal request deadline, not caller cancellation.
		ctx = context.WithValue(ctx, telegramRouteRootKey{}, context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot777:fixture/getUpdates", http.NoBody)
		require.NoError(t, err)
		resp, err := f.c.Do(req)
		require.NoError(t, err)
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Error(t, err)
		assert.Equal(t, "timeout", f.r.snapshot().LastError)
		assert.Equal(t, "response", f.r.snapshot().LastStage)
		assert.Equal(t, 1, f.r.snapshot().PollingFailures[0])
	})
}

func TestTelegramRoutesSDKPollingRemainsSequential(t *testing.T) {
	var f *routeFixture
	var inFlight, maxInFlight atomic.Int64
	var polls atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f = newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/getUpdates") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
			return
		}
		active := inFlight.Add(1)
		defer inFlight.Add(-1)
		if active > maxInFlight.Load() {
			maxInFlight.Store(active)
		}
		assert.NoError(t, req.ParseForm())
		switch polls.Add(1) {
		case 1:
			assert.Empty(t, req.Form.Get("offset"))
			w.Header().Set("Connection", "close")
			f.directFail.Store(true)
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":41}]}`)
		case 2:
			assert.Equal(t, "42", req.Form.Get("offset"))
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":42}]}`)
		default:
			assert.Equal(t, "43", req.Form.Get("offset"))
			<-release
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		}
	})
	f.r.interval = 10 * time.Millisecond
	f.r.logf = func(string, ...any) {}
	go f.r.run()
	defer f.r.close()
	bot, err := tbapi.NewBotAPIWithClient("777:fixture", tbapi.APIEndpoint, f.c)
	require.NoError(t, err)
	startedAt := time.Now()
	updates := bot.GetUpdatesChan(tbapi.NewUpdate(0))
	for _, id := range []int{41, 42} {
		select {
		case update := <-updates:
			assert.Equal(t, id, update.UpdateID)
		case <-time.After(15 * time.Second):
			bot.StopReceivingUpdates()
			releaseOnce.Do(func() { close(release) })
			t.Fatal("polling recovery timed out")
		}
	}
	bot.StopReceivingUpdates()
	t.Logf("sequential SDK polling recovered through CONNECT after %s (existing retry sleeps included)", time.Since(startedAt))
	releaseOnce.Do(func() { close(release) })
	select {
	case _, open := <-updates:
		assert.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("SDK polling did not stop")
	}
	assert.Equal(t, int64(1), maxInFlight.Load())
	assert.Equal(t, "reserve", f.r.snapshot().Selected)
}

func TestTelegramRoutesRealListenerJournalWorkerRestart(t *testing.T) {
	var bans, deletes, polls atomic.Int64
	f := newRouteFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Connection", "close")
		switch req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:] {
		case "getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":777,"is_bot":true,"first_name":"fixture"}}`)
		case "getChatAdministrators":
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "getChat":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":-100,"type":"supergroup","title":"fixture"}}`)
		case "getChatMember":
			status := "member"
			if bans.Load() > 0 {
				status = "kicked"
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"status":"`+status+`","user":{"id":7,"is_bot":false,"first_name":"fixture"}}}`)
		case "banChatMember":
			bans.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "deleteMessage":
			if deletes.Add(1) == 1 {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, `{"ok":`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage", "editMessageText":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":999}}`)
		case "getUpdates":
			polls.Add(1)
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		default:
			t.Errorf("unexpected fixture method: %s", req.URL.Path)
			w.WriteHeader(400)
		}
	})
	f.r.interval = 10 * time.Millisecond
	f.r.logf = func(string, ...any) {}
	go f.r.run()
	defer f.r.close()
	path := filepath.Join(t.TempDir(), "runtime.db")
	openJournal := func() (*moderation.Journal, *engine.SQL) {
		db, err := engine.NewSqlite(path, "fixture")
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		journal, err := moderation.NewJournal(context.Background(), db, moderation.DefaultLimits())
		require.NoError(t, err)
		return journal, db
	}
	j, db := openJournal()
	now := time.Now()
	plan := moderation.Plan{Key: "source:-100:7:user:7", ChatID: -100, IdentityID: 7, IdentityKind: "user", MessageID: 7, Destination: -200,
		Actions: []moderation.Action{{Kind: "ban", ChatID: -100, IdentityID: 7, IdentityKind: "user"},
			{Kind: "original_delete", ChatID: -100, IdentityID: 7, IdentityKind: "user", MessageID: 7, MessageDate: now.Unix()}}}
	_, _, fresh, err := j.Accept(context.Background(), plan, now)
	require.NoError(t, err)
	require.True(t, fresh)
	snapshot := func() map[string]moderation.Action {
		rows, e := j.Snapshot(context.Background())
		require.NoError(t, e)
		result := map[string]moderation.Action{}
		for _, a := range rows {
			result[a.Kind] = a
		}
		return result
	}
	initialDeadline := snapshot()["original_delete"].Deadline
	start := func() func() {
		api, e := tbapi.NewBotAPIWithClient("777:fixture", tbapi.APIEndpoint, f.c)
		require.NoError(t, e)
		listener := events.TelegramListener{TbAPI: api, RecoveryJournal: j, Group: "-100", AdminGroup: "-200",
			Bot: &mocks.BotMock{IsApprovedUserFunc: func(int64) bool { return false }}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- listener.Do(ctx) }()
		var once sync.Once
		stopListener := func() {
			once.Do(func() {
				api.StopReceivingUpdates()
				cancel()
				select {
				case e = <-done:
					require.ErrorIs(t, e, context.Canceled)
				case <-time.After(3 * time.Second):
					t.Error("listener/worker did not join")
				}
			})
		}
		t.Cleanup(stopListener)
		return stopListener
	}
	stop := start()
	require.Eventually(t, func() bool {
		a := snapshot()
		return a["ban"].State == moderation.Confirmed && a["original_delete"].Uncertain
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1), deletes.Load(), "ambiguous response is recorded before any journal retry")
	f.directFail.Store(true)
	f.r.transports[0].CloseIdleConnections()
	for range 3 {
		require.Error(t, f.request("getMe"))
	}
	require.Eventually(t, func() bool { return snapshot()["original_delete"].State == moderation.Confirmed }, 20*time.Second, 20*time.Millisecond)
	assert.Equal(t, int64(1), bans.Load())
	assert.Equal(t, int64(2), deletes.Load(), "only the journal schedules the second delete")
	assert.Equal(t, initialDeadline, snapshot()["original_delete"].Deadline)
	stop()
	require.NoError(t, db.Close())
	j, db = openJournal()
	defer db.Close()
	_, _, fresh, err = j.Accept(context.Background(), plan, time.Now())
	require.NoError(t, err)
	assert.False(t, fresh)
	beforePolls := polls.Load()
	stop = start()
	require.Eventually(t, func() bool { return polls.Load() >= beforePolls+30 }, 3*time.Second, 20*time.Millisecond)
	stop()
	assert.Equal(t, int64(1), bans.Load(), "confirmed ban survives reopen and listener/worker restart")
	assert.Equal(t, int64(2), deletes.Load(), "confirmed delete is not dispatched again")
	assert.Equal(t, initialDeadline, snapshot()["original_delete"].Deadline)
}
