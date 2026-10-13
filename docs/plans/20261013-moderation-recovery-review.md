# PR #2 review corrections

Base HEAD b06d36bafd68c6cde5249d0a41a67bf539223fd7, same feature branch.
Before edits: clean engine checkout, complete tracked-tree archive and head/status record at
C:\Users\User\Documents\Codex\backups\antispam-review2-20261013\engine-b06d36ba.zip.
Infra/compose/config/runtime DB untouched. Local rollback preserves this review commit and switches
to a separate clean checkout of the recorded base; never overwrite unrelated work or restore an old DB.
Production rollout requires a separate authorization and the existing backup/image/disk/down-up gate.

## 1. Report callbacks and worker

moderation_demands records provenance/accepted_at for each action, within the one journal.
Reject withdraws report authorities only; detector and forward contributions survive. Later report
batches use database-row identity after old votes are cleared. Callback receipt + authority changes
are committed in one SQL transaction. Approve submits/merges through the journal, with local learning
claimed non-replayably before completion. Independently ready detector work is not paused by report
preparation. Reporter-ban confirmation uses a callback-ID command receipt, no fictional source ID,
and the same serialized worker. No optimistic banned/deleted suffix is written to report cards.

Authority is checked in Worker and again before the actual Telegram mutation. Running requests are
not recalled; confirmed outcomes stay confirmed, lost responses remain manual/uncertain and are not
retried after withdrawal. Undo operations wait for running ban/restrict on the same gid/chat/identity.
Existing canceled work stays withdrawn after restart. Later independent originals preserve the
existing policy; new explicit requirements wait for unfinished undo/local operator preparation.

## 2. Existing source receipt is not equivalent to identical plan

Repeated admin delivery returns its existing receipt before reading a new plan. A different delivery
can append demands/actions to an exact source receipt. Known confirmed deletion pairs are reused;
additional cleanup IDs are persisted. Temporary -> permanent is a separate action, preserving the
old confirmed result. Permanent desired membership cannot be satisfied by a temporary kicked state.
Fixed temporary detector expiry is not slid by redelivery. New requirements do not reuse a historical
ban as coverage after later undo. Different originals with identical content retain different receipts.
No hash-based identity. Unknown exact source ID remains delivery-scoped with explicit locator limits.

## 3. Manual work remains independent of delivered notification

Prune excludes unresolved manual/failed/expired/blocked/uncertain actions and incomplete local
callback decisions even if card/digest were delivered. Close is an explicit operator resolution;
it neither claims API success nor rewrites the actual action outcome. Pending/running closure is
refused. Closed records still wait for age/delivery conditions. Unresolved records consume retained
capacity, producing a capacity warning instead of silent deletion. Prototype rows without provenance
are manual, not automatically assigned to a report or detector.

## 4. Individual cards, safe context and controls

moderation_cards stores original text/user context separately (32 KiB maximum), per receipt.
Cards contain source/candidate/command scope, identity, factual outcomes and operator-resolution
count. Digest remains a separate storm-suppressed warning surface. Controls are bound to the recorded
card/admin chat/gid. Info uses stored context/locator evidence. UpdateHam/UpdateSpam use stored original
text, not notification/status/digest text. Training/dry keep legacy callbacks and refuse new durable
mutation commands; worker runtime safety and cross-chat reliable-user fallback remain in place.

There is no exactly-once guarantee: an API acknowledgement can be lost before outcome recording.
Running withdrawal is not a promise to undo a sent request. Lost send acknowledgements can duplicate
cards; unacknowledged duplicate cards are not authorized to issue mutations until binding is known.
No automatic retries of permanent refusal without a new justified/operator request.

## Changed files / checks

New app/moderation/lifecycle.go + lifecycle_test.go, app/events/recovery_cards.go + tests.
Integration: journal.go, types.go, worker.go, recovery.go, admin.go, reports.go, listener.go.
Existing fallback test distinguishes notification destination from actual moderation target.
Offline migration probe now populates original-card/withdrawal/resolution data. Documentation updated.

Mocks cover independent report/detector rejection, new batch/new forward, duplicate delivery, upgraded
ban + cleanup IDs, temporary membership, callback-vs-running worker (confirmed and lost response),
restart after withdrawal, unknown provenance migration, operator closure/capacity/pruning after 31 days,
stored-text learning, card binding, notification facts, TTL, info, dry/training and safety gates.
Full Linux race/SQL/events/build/changed-code lint run in PR CI; old stage-1 binary is probed on local
copies of all three existing snapshots, without Telegram calls or production configuration.

Resource limits remain one worker/instance and 500 active/5000 unfinished/5000 retained receipts.
5000-action SQLite fixture with provenance: 2,592,768 bytes (~2.47 MiB), without full card bodies.
Worst-case raw original bodies: 5000 * 32 KiB = 156.25 MiB per instance on disk, plus metadata/WAL/
backups; worker does not load all bodies into RAM. Disk capacity must be checked before a separately
approved rollout. Actual VPS load was not benchmarked in this local review task. Individual cards
are bounded per receipt; network-warning storms still coalesce into one digest per destination.

The 5000-action constructor probe additionally exposed excessive per-action SQL reads under race
instrumentation. Plan registration now reads the minimal matching columns once per receipt in its
transaction, rather than issuing 50 full-row reads for a 50-action plan. Local native probe improved
from 5.26s to 1.25s; the fixture size and the existing 180s project timeout remain unchanged. Existing
receipt target identity is immutable, including command receipts; mismatch has a regression test.

## Follow-up: explicit approval and undo identity (review of 25fec60)

First ordinary report approval selects new runnable ban/delete actions instead of adopting the
manual_report_pending_decision placeholder. Explicit operator/report approval may accept a new
attempt after manual/failed/expired/blocked/canceled outcomes; old rows and codes remain unchanged.
Automatic plan redelivery continues to match terminal failures and never restarts them. Confirmed
operations and in-flight/pending operations are reused. The selected row ID is carried to demand
registration, including newly expired actions, avoiding ambiguous matching against historical rows.

Every fresh approval rechecks the existing event/adapter safety gates; actual API execution still
requires current authorization, membership and permission checks. Delete admission recalculates the
48h minus safety-margin deadline from Message.Date. Already expired deletes are recorded as expired
with deadline_manual_action_required and never sent; ban remains independent. Unknown dates keep
the documented bounded operation-horizon policy, never substituting observed_at for Message.Date.

Undo identity includes the callback decision ID; exact callback redelivery is fenced by the same
transactional decisions ledger, including after restart. New callbacks create independent undo
attempts after success or terminal failure; target serialization remains enforced. Explicit undo
supersedes terminal old undo work through operator-attributed resolution records, preserving old
API states/codes; the replacement pending/failed undo continues to block subsequent bans until
settled/resolved. Admission observes the shared action-capacity limit. No schema migration added.

Regressions cover first report R+ without R-, worker ban/delete, manual/failed/expired history,
ban/unban/new ban/second unban, failed undo then repaired confirmation, callback redelivery after
restart and already expired delete. Existing race tests cover callback/worker overlap.

Backup: Documents/Codex/backups/antispam-review2-followup-20261013/engine-25fec60.zip,
original files and clean Git state. Code rollback: separate clean checkout at 25fec60; do not reset
the shared working tree or restore runtime DB. Runtime rollback remains separately approved,
recovery-disabled old binary with DB/config/compose preserved as documented above. No production
changes, build/prune on VPS, merge or deployment performed.
