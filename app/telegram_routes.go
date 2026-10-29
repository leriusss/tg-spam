package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
)

const (
	telegramDirect        = 0
	telegramReserve       = 1
	telegramRouteFailures = 3
	telegramRouteInterval = 30 * time.Second
	telegramRouteDwell    = 2 * time.Minute
)

// Route credentials are read from a private file, never a CLI argument or log.
// HTTPS CONNECT keeps the Telegram TLS session end-to-end, including SNI.
func configureTelegramRoutes(ctx context.Context, client *telegramHTTPClient, token, proxyFile string) (func(), error) {
	if proxyFile == "" {
		return func() {}, nil
	}
	data, err := os.ReadFile(proxyFile) // #nosec G304 -- explicitly configured operator-owned credential file
	if err != nil {
		return nil, errors.New("telegram fallback: cannot read proxy credential file")
	}
	proxy, err := url.Parse(strings.TrimSpace(string(data)))
	if err != nil || proxy.Scheme != "https" || proxy.Hostname() == "" || proxy.User == nil ||
		proxy.User.Username() == "" || proxy.Path != "" || proxy.RawQuery != "" || proxy.Fragment != "" || proxy.Opaque != "" {
		return nil, errors.New("telegram fallback: expected authenticated HTTPS CONNECT proxy URL")
	}
	if password, ok := proxy.User.Password(); !ok || password == "" {
		return nil, errors.New("telegram fallback: proxy password required")
	}
	transports := [2]*http.Transport{}
	for idx := range transports {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil // Direct really means direct, independent of HTTP(S)_PROXY.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.TLSHandshakeTimeout = 5 * time.Second
		transport.ResponseHeaderTimeout = 90 * time.Second // Includes long polling; request context is the tighter budget.
		if idx == telegramReserve {
			transport.Proxy = http.ProxyURL(proxy)
			transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
				if resp.StatusCode == http.StatusProxyAuthRequired {
					return telegramProxyAuthError{}
				}
				return nil
			}
		}
		transports[idx] = transport
	}
	routes := newTelegramRoutes(ctx, token, transports)
	if client.cooldown == nil && client.pause == nil {
		// Without a journal, still share the complete rate limit across both routes
		// and probes. The production journal remains the durable owner when enabled.
		limiter := &telegramRouteCooldown{}
		client.cooldown, client.pause = limiter.load, limiter.store
	}
	routes.cooldown, routes.pause = client.cooldown, client.pause
	client.client = routes
	// Bounded active requests are required even when durable moderation is disabled.
	client.routeHTTP = true
	go routes.run()
	return routes.close, nil
}

type telegramProxyAuthError struct{}

type telegramRouteRootKey struct{}

type telegramRouteCooldown struct {
	mu    sync.Mutex
	until time.Time
}

func (c *telegramRouteCooldown) load(context.Context) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until, nil
}
func (c *telegramRouteCooldown) store(_ context.Context, until time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.until) {
		c.until = until
	}
	return nil
}

func (telegramProxyAuthError) Error() string { return "telegram fallback proxy authentication failed" }

type telegramRouteStatus struct {
	Selected           string       `json:"selected"`
	State              string       `json:"state"`
	LastSwitch         time.Time    `json:"last_switch"`
	SwitchReason       string       `json:"switch_reason"`
	LastExchange       time.Time    `json:"last_exchange"`
	LastPolling        time.Time    `json:"last_polling"`
	LastWorkerExchange time.Time    `json:"last_worker_exchange"`
	LastError          string       `json:"last_error"`
	LastStage          string       `json:"last_stage"`
	Failures           [2]int       `json:"failures"`
	PollingFailures    [2]int       `json:"polling_failures"`
	WorkerFailures     [2]int       `json:"worker_failures"`
	Probes             [2]string    `json:"probes"`
	ProbeAt            [2]time.Time `json:"probe_at"`
}

// One controller owns probes; Do never retries a request or starts a poller.
type telegramRoutes struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	wake          chan struct{}
	clients       [2]*http.Client
	transports    [2]*http.Transport
	token         string
	active        int
	status        telegramRouteStatus
	returnSuccess int
	nextProbe     time.Time
	probeDelay    time.Duration
	interval      time.Duration
	dwell         time.Duration
	now           func() time.Time
	logf          func(string, ...any)
	cooldown      func(context.Context) (time.Time, error)
	pause         func(context.Context, time.Time) error
}

func newTelegramRoutes(ctx context.Context, token string, transports [2]*http.Transport) *telegramRoutes {
	bounded, cancel := context.WithCancel(ctx)
	r := &telegramRoutes{ctx: bounded, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1),
		token: token, transports: transports, interval: telegramRouteInterval, dwell: telegramRouteDwell,
		now: time.Now, logf: log.Printf, probeDelay: telegramRouteInterval}
	r.status.Selected, r.status.State = "direct", "primary"
	r.status.Probes = [2]string{"not_checked", "not_checked"}
	for idx, transport := range transports {
		r.clients[idx] = &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse // No token-bearing redirects to another origin.
		}}
	}
	return r
}

func (r *telegramRoutes) close() {
	r.cancel()
	<-r.done
	for _, transport := range r.transports {
		transport.CloseIdleConnections()
	}
}

func (r *telegramRoutes) snapshot() telegramRouteStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *telegramRoutes) report() {
	data, _ := json.Marshal(r.snapshot())
	r.logf("[INFO] Telegram routes: %s", data)
}

func (r *telegramRoutes) Do(req *http.Request) (*http.Response, error) {
	// Constrain the credential-bearing transport to the official origin.
	if req.URL.Scheme != "https" || req.URL.Host != "api.telegram.org" || req.URL.User != nil {
		return nil, errors.New("telegram routes: unexpected API origin")
	}
	r.mu.Lock()
	idx := r.active
	r.mu.Unlock()
	stages := &telegramRouteTrace{}
	traced := req.Clone(httptrace.WithClientTrace(req.Context(), stages.trace()))
	resp, err := r.clients[idx].Do(traced)
	if err != nil {
		r.observe(idx, req, false, err, stages.stage())
		return nil, telegramRouteError{cause: err, class: telegramNetworkClass(err)}
	}
	// Count complete bodies, not just a successful TLS handshake/getMe headers.
	resp.Body = &telegramRouteBody{ReadCloser: resp.Body, buf: make([]byte, 0), finish: func(body []byte, readErr error) {
		if body == nil && readErr == nil {
			return
		}
		success := false
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && readErr == nil {
			var envelope struct {
				OK     bool            `json:"ok"`
				Result json.RawMessage `json:"result"`
			}
			success = json.Unmarshal(body, &envelope) == nil && envelope.OK
			if strings.HasSuffix(req.URL.Path, "/getUpdates") {
				success = success && len(envelope.Result) > 0 && envelope.Result[0] == '['
			}
		}
		r.observe(idx, req, success, readErr, stages.stage())
	}}
	return resp, nil
}

type telegramRouteBody struct {
	io.ReadCloser
	buf    []byte
	once   sync.Once
	finish func([]byte, error)
}

func (b *telegramRouteBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if len(b.buf)+n <= 4<<20 {
		b.buf = append(b.buf, p[:n]...)
	}
	if err != nil {
		b.once.Do(func() {
			if errors.Is(err, io.EOF) {
				b.finish(b.buf, nil)
			} else {
				b.finish(nil, err)
			}
			b.buf = nil
		})
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, telegramRouteError{cause: err, class: telegramNetworkClass(err)}
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, nil
}

func (b *telegramRouteBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if err != nil || json.Valid(b.buf) {
			b.finish(b.buf, err)
		}
		b.buf = nil
	})
	if err != nil {
		return telegramRouteError{cause: err, class: telegramNetworkClass(err)}
	}
	return nil
}

func (r *telegramRoutes) observe(idx int, req *http.Request, success bool, err error, stage string) {
	// Caller cancellation/shutdown is not evidence about a network path.
	if r.ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	root := req.Context()
	if original, ok := root.Value(telegramRouteRootKey{}).(context.Context); ok {
		root = original
	}
	if root.Err() != nil {
		return
	}
	class := telegramNetworkClass(err)
	r.mu.Lock()
	defer r.mu.Unlock()
	polling := strings.HasSuffix(req.URL.Path, "/getUpdates")
	method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	worker := !polling && !strings.HasPrefix(method, "get")
	if err == nil {
		r.status.Failures[idx] = 0
		if polling {
			r.status.PollingFailures[idx] = 0
		}
		if worker {
			r.status.WorkerFailures[idx] = 0
		}
	}
	if success {
		if idx == r.active {
			r.status.State = []string{"primary", "reserve"}[idx]
		}
		r.status.LastExchange = r.now()
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			r.status.LastPolling = r.now()
		} else if worker {
			r.status.LastWorkerExchange = r.now()
		}
		r.status.Failures[idx] = 0
	} else if class != "none" {
		r.status.LastError, r.status.LastStage = class, stage
		if class != "certificate" && class != "proxy_auth" && class != "other" {
			r.status.Failures[idx]++
			if polling {
				r.status.PollingFailures[idx]++
			}
			if worker {
				r.status.WorkerFailures[idx]++
			}
		}
	}
	if r.failureCount(idx) >= telegramRouteFailures && idx == r.active {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

// Call with mu held. getMe success must not erase a broken polling/worker lane.
func (r *telegramRoutes) failureCount(idx int) int {
	return max(r.status.Failures[idx], r.status.PollingFailures[idx], r.status.WorkerFailures[idx])
}

type telegramRouteError struct {
	cause error
	class string
}

func (e telegramRouteError) Error() string { return "telegram route: " + e.class }
func (e telegramRouteError) Unwrap() error { return e.cause }

func telegramNetworkClass(err error) string {
	if err == nil {
		return "none"
	}
	var auth telegramProxyAuthError
	if errors.As(err, &auth) {
		return "proxy_auth"
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "certificate"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "read"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return "tcp"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "tcp"
	}
	return "other"
}

type telegramRouteTrace struct {
	mu    sync.Mutex
	value string
}

func (t *telegramRouteTrace) set(value string) { t.mu.Lock(); t.value = value; t.mu.Unlock() }
func (t *telegramRouteTrace) stage() string    { t.mu.Lock(); defer t.mu.Unlock(); return t.value }
func (t *telegramRouteTrace) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { t.set("dns") },
		ConnectStart:         func(_, _ string) { t.set("tcp") },
		TLSHandshakeStart:    func() { t.set("tls") },
		GotConn:              func(httptrace.GotConnInfo) { t.set("request") },
		GotFirstResponseByte: func() { t.set("response") },
	}
}

// Probes only call getMe; never getUpdates, mutate offsets or retry user actions.
func (r *telegramRoutes) probe(idx int) bool {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	if r.cooldown != nil {
		until, loadErr := r.cooldown(ctx)
		if loadErr != nil {
			r.probeResult(idx, "cooldown_unavailable")
			return false
		}
		if r.now().Before(until) {
			r.probeResult(idx, "cooldown")
			return false
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+r.token+"/getMe", http.NoBody)
	if err != nil {
		return false
	}
	client := &telegramHTTPClient{client: r.clients[idx], recoveryHTTP: true, cooldown: r.cooldown, pause: r.pause}
	resp, err := client.Do(request)
	state := telegramNetworkClass(err)
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) {
		state = fmt.Sprintf("http_%d", apiErr.Code)
	}
	ok := false
	if err == nil {
		var envelope struct {
			OK     bool `json:"ok"`
			Result struct {
				ID    json.Number `json:"id"`
				IsBot bool        `json:"is_bot"`
			} `json:"result"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&envelope)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusProxyAuthRequired {
			state = "proxy_auth"
		} else {
			state = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		id, _, _ := strings.Cut(r.token, ":")
		ok = resp.StatusCode == http.StatusOK && decodeErr == nil && envelope.OK && envelope.Result.IsBot &&
			envelope.Result.ID.String() == id
		if ok {
			state = "ok"
		}
	}
	r.probeResult(idx, state)
	return ok
}

func (r *telegramRoutes) probeResult(idx int, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Probes[idx], r.status.ProbeAt[idx] = state, r.now()
}

func (r *telegramRoutes) run() {
	defer close(r.done)
	timer := time.NewTimer(r.interval)
	defer timer.Stop()
	r.report()
	for {
		ticked := false
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		case <-timer.C:
			ticked = true
		}
		checked := r.checkRoutes()
		if !checked && !ticked {
			continue
		}
		r.report()
		delay := r.interval + time.Duration(rand.Int64N(int64(r.interval/5)+1)) // #nosec G404 -- probe jitter
		r.mu.Lock()
		if next := r.nextProbe.Sub(r.now()); next > 0 {
			delay = next
		}
		r.mu.Unlock()
		timer.Reset(delay)
	}
}

func (r *telegramRoutes) checkRoutes() bool {
	r.mu.Lock()
	active, failures := r.active, r.failureCount(r.active)
	due := !r.now().Before(r.nextProbe)
	r.mu.Unlock()
	if !due || active == telegramDirect && failures < telegramRouteFailures {
		return false
	}
	primaryOK := false
	if active == telegramReserve {
		primaryOK = r.probe(telegramDirect)
	}
	reserveOK := r.probe(telegramReserve)
	if active == telegramDirect && !reserveOK {
		primaryOK = r.probe(telegramDirect)
	}
	if r.ctx.Err() != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if primaryOK {
		r.returnSuccess++
	} else {
		r.returnSuccess = 0
	}
	canSwitch := r.status.LastSwitch.IsZero() || r.now().Sub(r.status.LastSwitch) >= r.dwell
	switchTo := active
	reason := "primary_network_failures"
	switch {
	case active == telegramDirect && reserveOK && canSwitch && r.failureCount(active) >= telegramRouteFailures:
		switchTo = telegramReserve
	case active == telegramReserve && r.returnSuccess >= telegramRouteFailures && canSwitch:
		switchTo, reason = telegramDirect, "primary_recovered"
	}
	if switchTo != active {
		r.active = switchTo
		r.status.Selected = []string{"direct", "reserve"}[switchTo]
		r.status.LastSwitch, r.status.SwitchReason = r.now(), reason
		r.status.Failures[switchTo] = 0
		r.status.PollingFailures[switchTo], r.status.WorkerFailures[switchTo] = 0, 0
		r.returnSuccess = 0
		// Active requests retain their bounded contexts. Only new requests change route.
		r.transports[active].CloseIdleConnections()
	}
	if r.active == telegramReserve {
		r.status.State = "reserve"
	} else {
		r.status.State = "primary"
	}
	limited := r.status.Probes[0] == "cooldown" || r.status.Probes[1] == "cooldown" ||
		r.status.Probes[0] == "http_429" || r.status.Probes[1] == "http_429"
	if !reserveOK && !primaryOK && !limited {
		r.status.State = "unavailable"
	}
	if r.status.State == "unavailable" {
		r.probeDelay = min(2*r.probeDelay, 5*time.Minute)
	} else {
		r.probeDelay = r.interval
	}
	r.nextProbe = r.now().Add(r.probeDelay + time.Duration(rand.Int64N(int64(r.probeDelay/5)+1))) // #nosec G404 -- jitter
	return true
}
