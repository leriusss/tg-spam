# Moderation recovery operation / rollback

This feature PR authorizes no production deployment. Base rollback binary is commit
44d5999c44554acef747d7e44ed11709ea513212; existing image
sha256:3b858d4fffc4f88cd10e0f125db1a752b9d66a17850397ad7f92a964c13168db.
Never change existing compose diff, routing, shared-admin policy or global bans in this release.

## Review and local verification

Read implementation specification and tests. Run go test -race -short ./app/moderation ./app/events
./app/storage ./app, then go test -race -timeout=300s ./..., go build -race ./... and changed-code lint
against the base commit on Linux with Docker available for PostgreSQL. Windows lacks a C compiler
for race instrumentation; Linux CI is the race/PostgreSQL verification authority.
Build base and tools/moderation-migrate-probe locally. Run tools/verify_moderation_rollback.py --help
for exact copy-only arguments; pass offline SQLite snapshots, never live production DB paths.
The probe creates and migrates its own temporary copies, disables Telegram, and starts the base
executable with local web auth and pruning disabled. No customer rows are printed or committed.

## Separately approved rollout

1. Check logs, polling, git/image identity, free space and archive checksums. Preserve old image,
   current configs, compose file AND tracked diff, full logs, consistent SQLite backups of all instances.
2. Verify old binary/new schema on copies and inspect all CI checks. Identify exact gid per instance.
3. Enable MODERATION_RECOVERY=true explicitly only after reviewing individual cards, callback authority
   and retained-work operations. Keep
   existing shared-admin reliable-user fallback: each instance can still act in its own primary chat.
   Feature flag off is legacy behavior, not a retry guarantee.
4. Publish reviewed image once, restart only docker-compose down / docker-compose up -d.
5. Check container start, schema/integrity, legacy row preservation, polling and naturally arriving
   updates. Do not create real test bans. Observe independent journal action transitions and notices.
6. Roll back on startup/schema/crash/polling/moderation regressions, not an isolated recovered reset.

## Read-only operator queries

SQLite times are UTC epoch milliseconds except message_date and until_date (epoch seconds).
Open an offline snapshot or use SQLite read-only mode. Do not print owner tokens or message content.

```sql
SELECT gid,kind,state,code,uncertain,count(*) FROM moderation_actions
GROUP BY gid,kind,state,code,uncertain;
SELECT gid,receipt,kind,chat_id,identity_kind,identity_id,message_id,message_date,state,code,attempts,
       deadline,next_at,lease_until,uncertain
FROM moderation_actions WHERE state NOT IN ('confirmed','absent') ORDER BY next_at;
SELECT gid,generation,delivered,state,code,attempts,notification_id
FROM moderation_actions WHERE kind='notice';
SELECT gid,cooldown,rejected FROM moderation_control;
```

Unknown date is message_date=0; expired/manual/absent/uncertain are not confirmed deletions.
Notice generation>delivered is an undelivered signal even after retries end. Permanent 400/401/403/
404/501 delivery refusals are not revived by new events; repair destination/permissions, then require
an explicit operator-reviewed recovery procedure. This PR exposes no automatic permission bypass,
manual claim reset or bulk replay command. Export pending IDs/results privately before rollback.
The digest is one evolving message per destination, <=one attempt/minute, with counts and ten latest
non-confirmed IDs; detailed per-item state remains in SQL. A new terminal result receives its own
24-hour/320-attempt delivery window, even if the earlier incomplete-warning window was consumed.
Repeated delivery failures alone never extend that window. Lost send ack can duplicate a notification.

## Runtime rollback, keep current DB

Preserve current runtime DB, receipts, pending signals, compose diff and configs before switching.
Stop the new worker via docker-compose down; select the verified previous image and previous config
with MODERATION_RECOVERY unset/false while preserving all unrelated local compose edits. Then run
docker-compose up -d, verify actual container image IDs, start logs, integrity and polling.
Do not restore older DB snapshots during ordinary rollback: that would lose new runtime data.
The additive moderation tables are readable by the old binary, but it does not drain them. Export
unfinished jobs and arrange manual moderation. Later roll-forward must recheck current gates and
actual deadlines; only expired leases are reclaimed, never confirmed actions.
Local code rollback: preserve the feature commit, then switch to a checkout of the base commit.

## What this prevents, and what it does not

For an admitted future incident like 10 October, temporary automatic or admin-forward API failure
no longer disappears into a log: the exact action is persisted, separately from a successful ban,
and retried after recovery/restart before its deadline. Cleanup items and incomplete/final notices
have independent outcomes. It does not extend Telegram's deletion window, reconstruct unknown
original IDs, retry permanent refusal, backfill the old incident, or promise exactly-once API effects.
Routing/global-ban decisions and explicit manual resolution after permanent failure
remain explicit review/policy choices; this implementation does not disable reliable-user fallback.

## Review revision: individual cards and operator decisions

Individual cards use opaque callback references bound to gid/admin chat/notification message ID;
original text is in moderation_cards, not recovered from rendered statuses. The digest remains for
coalescing warnings. Info shows original detection evidence when still available, otherwise says it
is unavailable. Display truncation does not truncate UpdateHam/UpdateSpam input. Commands without
an exact original ID explicitly say so; a locator candidate is not an exact forward identity.

Reject report removes current report authority, not detector/forward authority. Report votes are
cleared as before while durable original context remains usable after report-storage TTL. A later
report batch has a new database report-row identity. Approve and reporter-ban confirmations enter
the same action journal; reporter commands do not invent an original message ID or learn metadata.
Manual-threshold card binding is adopted; original reports/cards do not assert API success when queued.

Cancel retries withdraws existing authorities for that target in the current primary chat/gid.
The cutoff persists across restart; later independent originals remain subject to the existing
moderation policy. Unban additionally queues the undo in the journal and retains the existing explicit
user-approval/ham contract. Running effects may already have reached Telegram. Undo cannot overtake
running ban/restrict; a new requirement waits for unfinished undo/local operator preparation. No global
ban or cross-instance routing is introduced. Explicit approvals are new authorities, not replay of
old delivered callbacks. The prior temporary ban stays recorded when a permanent requirement is added.

Mark manually resolved work only after checking the actual chat/API situation. Close is refused for
pending/running core actions, retains actual failed/expired/manual/uncertain results, and records the
operator ID/time in moderation_resolutions. Notification delivery never closes work. After 30 days,
pruning requires no unresolved manual/failed/expired/blocked/uncertain actions, no started local
operator decisions and acknowledged card/warning delivery. Retained receipts still count toward the
5000 cap until eligible for pruning; unresolved work is never discarded to free capacity.

```sql
SELECT receipt,action_id,origin,active,accepted_at FROM moderation_demands WHERE gid=?;
SELECT receipt,operator_id,verb,state FROM moderation_decisions WHERE gid=?;
SELECT receipt,action_id,operator_id,closed_at FROM moderation_resolutions WHERE gid=?;
SELECT chat_id,identity_id,created_at FROM moderation_holds WHERE gid=?;
```

accepted_at/hold cutoff are processing authority times, not Telegram Message.Date. Raw body text
must not be included in public logs or review artifacts. New tables are additive. Prototype jobs
without provenance become manual review rather than guessing report-vs-detector origin. Do not run
the old b06d36ba worker against this extended lifecycle: for code rollback disable recovery, preserve
runtime DB/holds/pending decisions and export manual work. The production stage-1 binary remains the
separately approved image rollback target and ignores the extra tables; ensure existing approved-user
policy remains enforced while the new worker is disabled. No rollback/deploy has been performed here.
