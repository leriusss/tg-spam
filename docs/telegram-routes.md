# Telegram Bot API route failover (disabled by default)

The existing HTTP client owns two independent connection pools and one probe
controller. There is no sidecar, extra polling consumer, offset store, new
moderation retry queue, migration, or change to moderation policy. This follows
the transport incident recorded in private infrastructure issue
`leriusss/atlas-antispam#1` (27 October 2026).

## Configuration and independent egress

Leave `TELEGRAM_FALLBACK_PROXY_FILE` unset to preserve existing client behavior.
To opt in, mount an operator-owned read-only credential file, mode `0600`, and
set `TELEGRAM_FALLBACK_PROXY_FILE=/run/secrets/telegram-egress-url`.
The file contains a URL of this form (placeholder values only):

```text
https://BOT_SPECIFIC_USER:URL_ENCODED_PASSWORD@OWN_PROXY_HOST:443
```

Only authenticated **HTTPS CONNECT** is accepted. The outer TLS session verifies
the proxy hostname; the inner TLS session verifies `api.telegram.org`, using the
normal system trust store. No TLS interception, custom Telegram IP, random proxy
discovery, MTProto, tg-ws-proxy or local Bot API server is introduced. The direct
pool ignores ambient `HTTP_PROXY`/`HTTPS_PROXY`; fallback-disabled behavior stays
unchanged. Redirects are refused on both enabled routes. Credentials must not be
passed as CLI values, committed, or printed by configuration/debug tooling.

Before a future rollout, provision/reuse an owner-controlled HTTPS CONNECT proxy
on an independently routed network. Require certificate validation, per-bot
authentication, source allowlists where feasible, and an ACL permitting only
`api.telegram.org:443` CONNECT. It must forward opaque TLS and resolve Telegram
from its own network, rather than loop through the affected VPS. Protect its
logs: CONNECT hostname is sufficient; never record inner traffic or credentials.
Avoid placing a proxy behind the same provider/route bottleneck. Set explicit
resource limits and maintain its certificate and credentials.

Read-only inventory on 29 October found a second existing owned server where a
strict-TLS request to the old Telegram IP succeeded. No authenticated CONNECT
service was established or validated there, and nothing was installed. This
server is only a candidate egress, not a working production fallback. No paid
resources were ordered. Independent internet failover remains **unverified**.

## State machine and request safety

- Start on `direct` / `primary`; the reserve pool is unused while direct traffic
  works. Three consecutive transport failures trigger one reserve `getMe` probe.
  The probe must return HTTP 200, valid `ok=true`, `is_bot=true`, and the expected
  numeric bot ID before switching. It never calls `getUpdates`.
- Network errors are classified as DNS, TCP, TLS-stage timeout, timeout or response
  read failure. Certificate failures and proxy authentication errors are separate
  fail-closed diagnostics. HTTP 401/403/409/429 and other HTTP refusals do not count
  as network failures. A complete HTTP exchange breaks that lane's failure streak.
- `getUpdates`, worker mutations (non-`get*` methods) and all traffic have separate failure streaks.
  A working `getMe` cannot erase a broken polling/worker lane. A complete empty
  `ok=true,result=[]` response updates the polling-success timestamp. Silence in
  the chats is not a failure signal. Probes do not update traffic timestamps.
- While on reserve, probe the primary every 30 seconds plus 0–20% jitter. Return
  only after three successive successful primary probes and at least two minutes
  since the previous switch. No immediate oscillation. Probe state is distinct
  from polling and worker state; a healthy primary `getMe` is not proof that its
  next long poll will work. Continued lane failures can trigger a later failover.
- If both routes fail, expose `unavailable`, retain the selected route and use
  one serial probe loop with exponential delays from 60 seconds to five minutes,
  plus jitter. Resume normal probe cadence when a path recovers. There is never
  a parallel stream of probes, and repeated request failures cannot bypass the
  next-probe time. Traffic itself retains its existing caller/journal backoff.
- Every original request is submitted once. A switch affects subsequent requests
  only. Old active calls finish/cancel under their request contexts: 15 seconds
  for ordinary methods, 90 seconds for polling; startup still has five attempts,
  15 seconds per attempt and 90 seconds total. No fallback replay is inserted
  into these budgets. DNS/TCP and TLS setup have five-second limits. Closing idle
  connections alone is not used as cancellation of active requests.
- With fallback enabled, startup also treats local DNS NXDOMAIN as retryable
  within the same finite attempts/budget: an independent proxy can resolve the
  hostname from its own network. Without fallback, startup classification is
  unchanged. Certificate and authentication refusals still fail closed.
- Explicit caller cancellation/deadline and application shutdown do not penalize
  a path. Internally generated request timeouts do. The controller is canceled
  and joined on application exit, including startup failure.
- Both traffic and probes respect the same durable cooldown. A probe's 429 also
  persists the full Retry-After through the existing client; fallback cannot
  bypass Telegram's rate limit. No moderation operation is replayed by route
  selection. Typed underlying causes survive safe errors for existing recovery
  handling; an accepted operation with an ambiguous response remains uncertain.

If durable recovery is disabled, the opt-in transport still shares an in-memory
cooldown across both pools/probes and keeps the full Retry-After. Only the
existing journal makes cooldown persistent across process restart.

The SDK still owns the single sequential polling loop and its in-memory offset.
Journal receipts, actions, claims, decisions, retry authority and immutable
delete deadlines are untouched. A process restart does not restore route state
(it starts direct); durable moderation state remains in the existing database.

## Diagnostics

The controller emits safe JSON under `Telegram routes:` periodically and after
probe cycles/switches. Fields include selected route, state, last switch time and
reason, last completed successful Bot API exchange, last successful polling and
worker response, last error class/stage, per-lane counters, probe outcomes and
probe times. The stages describe DNS/TCP/TLS/request/response when available;
reused connections naturally do not perform fresh DNS/TLS. Probe outcomes never
substitute for polling timestamps. Initially unknown timestamps are zero.

Only enums, counters and times are rendered. No URL, hostname supplied by an
error, proxy password, token, message text or personal ID is logged. Response
inspection uses a bounded in-memory buffer (4 MiB) that is discarded on close;
oversize/invalid envelopes may leave success timestamps unknown, never affect
the SDK response or moderation decisions. Existing unrelated debug logging is
outside this change; do not enable token-bearing SDK request debug in production.

## Tests and their limits

`go test -race -v ./app -run '^TestTelegramRoutes'` uses the actual application
client and SDK with local TLS servers, a fixture CA and authenticated HTTPS
CONNECT. No `InsecureSkipVerify` is used. The proxy asserts the CONNECT target,
the API asserts Telegram SNI and absence of Proxy-Authorization at the origin;
distinct listener counters prove where connections actually terminate.

Coverage includes healthy direct traffic, isolated/persistent failures, broken
reserve/backoff, controlled return/dwell, DNS/reset/read/TLS timeout, invalid
certificate, cancellation, HTTP refusals/429, proxy authentication, startup,
single sequential SDK polling with preserved offset, ambiguous mutations without
transport replay, active old calls versus new-route calls, and credential-safe
errors. A real listener/journal/worker test records an ambiguous delete response,
switches the actual client through CONNECT, leaves the retry to the journal,
then reopens the same SQLite and restarts listener/worker: confirmed ban/delete
are not repeated and the original delete deadline is unchanged. Existing full
journal/listener/restart suites remain required.

`tools/check_telegram_route_nat.sh` creates a new disconnected Linux network
namespace on a disposable runner. Two local verified TLS endpoints deliberately
serve different payloads; OUTPUT DNAT changes the response even with the same
`curl --resolve`. Endpoint payload and NAT counters prove the rewritten path.
It never changes the host namespace. Do not run fault injection on production.

Fixture timing is not internet recovery timing: local failover after the third
immediate failure takes milliseconds; the SDK's existing three-second polling
retry sleeps add approximately nine seconds in the sequential polling test.
Real failures can consume full request deadlines and probe budgets. No live
independent proxy outage/recovery, production switchover or fresh card delivery
through such a proxy is claimed by these tests.

## Current read-only status (29 October 2026, 13:18 UTC / 15:18 Kaliningrad)

All three containers were running on the previously deployed image
`sha256:f7be3dc42aec7266cb0e1f7e810e26425c060e7497a9e7bb5dd491797200df50`,
restart count zero. In the preceding 30 minutes no transport/worker errors were
found; card rows were all confirmed (Baltia 193, Europa 21, Neoport 22); SQLite
quick checks were `ok`. No synthetic moderation or forwarding was performed.

The temporary DNAT service was still active and redirected
`149.154.166.110:443` to `149.154.167.220:443`. Strict-TLS HEAD probes on this VPS
returned 302 through that current route. They do **not** establish direct access
to the old IP. A strict-TLS direct probe from the second owned server returned
302 (TCP ~33 ms, TLS ~143 ms). No namespace without the rule existed on the bot
VPS. Direct recovery on that specific VPS remains unproven; its firewall was not
changed to test it. No production runtime or network configuration was modified.

## Future transition from temporary DNAT (separate approval required)

1. Review/merge this PR only after all CI suites pass. Build/pull a verified
   multi-architecture image by digest. Confirm independent egress ownership,
   ACL, authentication, outer and inner TLS and real getMe for each bot through
   the proxy without diagnostic getUpdates or real moderation actions.
2. Back up actual image/config/mounts/compose diff and consistent **current** DBs,
   with journal exports, integrity checks, hashes and an off-host copy. Preserve
   all policies and runtime data. Prepare exact rollback before rollout.
3. Validate the primary from an independently controlled network namespace or
   separate approved probe host whose path demonstrably bypasses the DNAT; do
   not label `--resolve` or reported peer IP on the affected VPS as proof. A
   different server's result does not establish the bot VPS's direct route.
4. Enabling fallback while DNAT remains active still makes the app's logical
   direct path use the temporary address. Report this explicitly. Only after
   independent reserve verification and separately approved transition remove
   the exact incident rule, verify absence/errors and validate actual direct
   traffic. Do not restore a whole firewall snapshot. The current helper's remove
   loop can confuse an iptables error with absence: require an independent rule
   check and explicit error handling in that future infrastructure change.
5. Stage one instance, then the remaining two with the same verified image, under
   the future authorized deployment procedure. Keep offsets/history; never run a
   second poller or reset/drop Telegram's queue. Observe 15 minutes (each minute
   for the first ten, then at minute 15): polling completions, route state,
   natural card deliveries, errors, cooldown, expired claims, DB integrity,
   resources and restarts. No synthetic spam, test bans or callbacks. If no
   natural events occur, fresh live delivery remains unverified.

Rollback of a future application rollout: disable the optional proxy-file setting
and return to the previously verified image with the **current** runtime DBs and
journal, never old snapshots. Stop workers via the agreed container procedure
before changing the image. No schema rollback is needed for this feature. Export
unfinished/uncertain actions, retain their deadlines and existing recovery policy,
then let the compatible worker resume; never blindly replay them. Network rollback
is a separate decision: retain/reapply only a reviewed exact incident rule if it
is required for connectivity; do not automatically disable a working workaround.
No production rollout, firewall change, image switch or rollback is part of this PR.

References: [Go HTTP transport](https://pkg.go.dev/net/http#Transport),
[CloseIdleConnections](https://pkg.go.dev/net/http#Transport.CloseIdleConnections),
[Telegram getUpdates](https://core.telegram.org/bots/api#getupdates).
