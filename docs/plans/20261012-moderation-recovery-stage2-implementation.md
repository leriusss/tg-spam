# Stage 2 implementation specification

Base: production `44d5999c44554acef747d7e44ed11709ea513212`. Local branch
`codex/moderation-recovery-stage2`; no production changes or VPS Docker operations.
Original tracked files are archived at
`C:\Users\User\Documents\Codex\backups\antispam-recovery-stage2-20261012\engine-base-44d5999.zip`
(SHA256 `32be581bf81269b87f2807f7c5b4fdda1dae9e136d76f2945712fe21276de881`).
Engine checkout was clean. Infra untracked harnesses and existing compose changes are not touched.

## Actual integration points and retry inventory

`listener.procEvents` performs automatic ban, extra deletion and original deletion synchronously.
`admin.MsgHandler` and `msgHandlerFallback` perform learning before deletion/ban and dispatch
best-effort asynchronous cleanup. `send` immediately retries another parse mode for every failure,
including transport failures. Vendored GetUpdatesChan retries polling after 3 seconds, not these
moderation actions. The token-sanitizing HTTP client has no whole-request timeout.
These polling retries could not recover the failed automatic ban/delete calls on 10 October.

Use one additive SQL journal: receipts, deliveries, actions, and a per-instance control row.
The queue is unfinished actions in that journal; notifications are coalesced notice actions in
the same journal. Creation, delivery binding, independent outcomes and notice dirtying are atomic.
No mutation is permitted when acceptance or ownership cannot be persisted.

Exact automatic identity is `(gid, chat_id, original_msg_id, identity_kind, identity_id)`.
A forwarded channel origin with matching chat/message ID can attach to that same receipt.
User/chat origins and text/media locator candidates do not establish exact forward identity:
use `(gid, admin_chat_id, admin_message_id)` for delivery-scoped receipts. A redelivery is suppressed;
a new forward without an exact original ID remains distinct. Never deduplicate by content hash.
Preserve existing primary-chat targeting, reliable-user fallback and all safety locks.

Learning is non-replayable: persist preparation ownership before approval removal/learning,
activate API actions after learning returns, and mark an interrupted preparation for manual review.
Do not store message content just to retry learning. Automatic detection uses the same journal
without an extra learning action. The community `/report` auto-threshold also uses this journal: its existing learning is best-effort,
so failed/interrupted learning produces a manual learning note and resumes core moderation, without
replaying learning. Direct commands and manual report callbacks retain their existing contract.

## Worker and safety

One worker per instance, shared by all durable actions, 5-second idle polling and a minimum
1-second interval between actions. Default limits: 500 active receipts and 5,000 unfinished API
actions per instance, plus 5,000 retained receipts. Cleanup overflow becomes a per-item manual result, preserving admitted core ban/original-delete.
Core/receipt overflow fails closed and creates a durable coalesced capacity signal;
no unpersisted API action runs. Completed receipts/delivery records have independent 30-day
retention; unfinished actions and undelivered terminal signals are not pruned.

Each HTTP action has a 15-second context deadline; 90-second fenced leases have takeover grace.
Before API calls revalidate dry/training, current explicit approval, configured/admin superusers,
target validity and allowed chat. Record independent ban/original/each-cleanup results. Preserve
fixed temporary-ban expiry; never accidentally turn an overdue short ban into a permanent ban.
Confirmed persisted actions are never replayed while the receipt is retained. Lost ban responses
require membership reconciliation; sender-chat bans cannot be reconciled and become manual-review
when the API outcome is uncertain. Exact-pair delete replay can return absent, which is not proof
of deletion by this bot. Leases cannot fence Telegram; no exactly-once claim is made.

## Retry schedule and dates

320 total attempts, exponential 2-second start, 10-minute cap, equal jitter [0.5,1], 24-hour horizon.
Honor 429 retry_after as a minimum and persist bot-wide cooldown; do not shorten it to fit a budget.
Eligible 500/502/503/504 and transport failures are retried; permanent refusals are terminal.
The first reached limit wins. Delete deadline is min(operation horizon,
Message.Date + 48 hours - 2 minutes), leaving room for the whole request timeout.
Unknown/future dates are unknown, not observed_at: use the operation horizon, keep the unknown-date
flag visible, and stop on a permanent refusal. Do not enqueue historical incidents automatically.
Tests simulate min/max jitter, request time and retry_after across the full 24 hours.

## Durable signals

One rate-limited notice action per instance/destination summarizes independent receipt results.
Transport failure updates a pending signal rather than producing a message on every attempt.
Terminal/deadline/budget/manual-review events remain queryable even if delivery fails. Notice
delivery has its own bounded period that can outlive the delete deadline. A new terminal
outcome starts a fresh 24-hour/320-attempt delivery window; ordinary retry failures do not renew it.
An older in-flight notification cannot consume the new terminal generation's delivery budget. Generation fencing
preserves newer changes while an older digest is in flight. Lost send acknowledgements may cause
duplicates; known notification message IDs allow later edits. No tokens or raw URL-bearing errors are persisted. Original text is stored only in the bounded
card-context table, separately from statuses and digests. Operator queries document undelivered signals and pending IDs.

## Files and checks

New `app/moderation/{journal,worker,types}.go` and tests; integration
`app/events/recovery.go`, `listener.go`, `admin.go`, `reports.go`; wiring `app/main.go`; transport/error tests
and implementation/runbook documentation. No vendor edits, routing changes or production compose edits.
Run meaningful worker/SQL/event mocks, concurrent SQL claims, restart/lease tests, full Linux/race/PG
project checks, and old stage-1 executable against copies of migrated SQLite fixtures.

## Rollback / future deployment gate

This PR is local only. Local rollback switches to the base commit (after preserving feature work).
Before any separately approved deployment: disk check, verified old/new image, consistent SQLite
backups, full logs and config/compose diff; down/up only. Runtime rollback uses the stage-1 image
and config with current DB retained. Extra tables are additive and old binaries ignore them.
Stop the worker before rollback; old binary will not drain pending jobs, so export pending/undelivered
states and arrange manual action. Do not restore older DB snapshots and lose live data. Rolling
forward reclaims only expired leases, rechecks gates/deadlines and resumes unfinished actions.

## Verification and limits

Migration was checked on local copies of all three existing SQLite snapshots. It preserves every
legacy row, integrity_check is ok, and the original stage-1 executable reads five offline web
endpoints and exports the additional tables without changing unfinished actions. The probe disables
Telegram and normal locator TTL pruning to isolate schema compatibility. No production DB is opened
by the probe. Scripts: tools/moderation-migrate-probe and tools/verify_moderation_rollback.py.

Durable mode is opt-in, MODERATION_RECOVERY=false by default. Missing admin destination fails startup
when enabled. Training/dry retain the existing synchronous review buttons and their original text,
callback IDs, confirmation/rejection/info contract; destructive journal admission is skipped.
Enabled normal moderation restores an individual persistent card for each receipt, with original
text/user context, factual action states and bound controls. A separate coalesced digest carries
incomplete/final warnings. Original text is retained separately in moderation_cards (maximum 32 KiB)
and UpdateHam/UpdateSpam read that stored original, including when presentation is truncated.
Training/dry retain their existing review workflow; source/routing/global-ban policy is unchanged.
See the [review corrections](20261013-moderation-recovery-review.md).

No recovery guarantee when the journal cannot be written, when capacity is exhausted, after the
first deadline/budget limit, for permanent Telegram refusal, or for learning that cannot safely be
replayed. Unknown date is explicitly 0 in SQL; a permanent delete refusal is manual-required, not
proof of age. Reconciliation proves current desired user membership, not authorship of a previous
ban; uncertain sender-chat ban requires manual review. Deleted-pair confirmation is shared inside
one gid only. Receipt retention bounds dedupe; after safe pruning a repeated historical delivery
cannot be recognized. Unknown original ID only suppresses the same admin delivery, not a new forward.

The VPS read-only resource check found 2 vCPU and about 2 GiB RAM. There is one worker per instance,
no parallel action calls; at most one action start per second plus current-identity preflight, idle
query every five seconds. Three instances therefore add <=0.6 idle SQL queries/second in aggregate.
Limits apply per instance, not global. No queue of full message bodies in RAM. Notice detail is bounded
to 10 rows with SQL aggregates; one notice per destination, >=60 seconds between delivery attempts.
Receipts/actions are disk metadata; history is pruned hourly only after 30 days and notice delivery.
Undelivered/manual work is retained, and retained-receipt capacity fails closed rather than discarding it.
The bot-wide persistent 429 barrier covers worker and polling HTTP requests. getUpdates has a 90-second
whole-request ceiling in durable mode; other HTTP calls have 15 seconds, including response-body read.
A long retry_after is honored and may mean expiry before another API attempt. 320 is a ceiling,
not an entitlement: schedule simulations cover 24h jitter extremes and time spent in requests.

See [operations runbook](../operations/moderation-recovery.md) for the deployment and rollback gate.

Local focused moderation/event tests and new transport/config tests passed. The 5,000-action SQLite
load probe used 2,592,768 bytes of database pages (~2.47 MiB), including demand provenance for metadata; this is a local fixture,
not a claim about production total DB size, CPU, disk latency, or WAL/retained-history footprint.
The full Windows suite has pre-existing Linux filesystem/server assumptions, and the full storage
suite has a timeout expectation failure also reproduced on the unmodified base. Those tests are
not weakened; full Linux/race/PostgreSQL results must be checked on the feature PR.

The full race checks exposed a pre-existing polling-log test cleanup race: it waited for the first
of two SDK log lines and restored a global logger before the polling goroutine stopped. The test now
waits for the retry line and joins the SDK's closed updates channel before logger restoration. This
changes test synchronization only; no SDK/vendor code, assertions or production polling policy changed.
