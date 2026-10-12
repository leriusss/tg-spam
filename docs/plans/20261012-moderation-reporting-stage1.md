# Moderation reporting: stage 1 implementation and review

Status: local implementation, production deployment not authorized.

## Scope and limits

Admin forwards (text and captionless media), reliable-user fallback, direct `/spam` and `/ban`,
automatic detector moderation, and their cleanup report independent ban and original-delete outcomes.
Feedback follows API actions; dry/training paths never claim confirmed mutations. Existing origin,
identity, approval, superuser, channel and anonymous-admin safety gates remain in place.
The shared admin-chat policy remains unchanged: reliable-user locator misses still target each
instance's own primary chat. No routing switch, persistent receipt, worker or retry is enabled.

This stage fixes reporting. It does **not** recover moderation or report delivery after a network
failure or restart. Asynchronous cleanup remains best-effort and can be interrupted by shutdown.
User-report aggregation/callback moderation and `/warn` are outside this incident-focused change.

## Reporting contract

- `confirmed`: Telegram acknowledged the action. Soft bans explicitly say permissions restricted.
- `failed`: a refusal or execution error; safe cause is shown where available.
- `unknown`: transport/response failure means the action's result is not established.
- `not_attempted`: dry/training/safety gate, learning failure, or unlocated fallback original.
- Delete `expired`: a known Telegram send date exceeds the 48-hour window; manual deletion required.
- Delete `absent`: absent/inaccessible, never described as confirmed deletion by this bot.

Ban and delete failures are accumulated independently. An original-delete refusal is not called
`spam notification failed`. Report delivery failures use `notification_failed` and do not erase
successful action results. Cleanup returns selected/deleted/absent/expired/failed/unknown/remaining
counts, per-message results and aggregate errors, even when some deletions succeeded.
The initial report says cleanup pending; an asynchronous final report includes zero-deletion and
partial outcomes. Detailed Telegram feedback is bounded; every attempted item is logged separately.

## Dates and additive storage

`message_dates` stores `(gid, chat_id, msg_id, message_date, observed_at)` using Unix UTC seconds.
`message_date` comes from Telegram `Message.Date`; `observed_at` is the first local observation.
Edits preserve the first known date. The existing locator `messages.time` retains its processing
time semantics and existing lookup/TTL behavior. Legacy rows have unknown send dates; they are not
backfilled from processing time or ambiguous `ForwardOrigin.Date`. A future/invalid date is unknown.
Known expired deletions are skipped; unknown dates are submitted to Telegram once without guessing
the cause of a refusal. Orphan date rows are pruned with locator registration, using a supporting index.

The additive date capability does not change the existing Locator interface or generated mocks.
The production SQL locator implements it; older/test locators without it have unknown send dates.
Old binaries can ignore the extra table/index. This is metadata, not a receipt or durable work queue.

## Before-change evidence and backup

Base engine commit: `e2817510648e67d88270247cf267a0a56adb7013`.
Feature branch: `codex/moderation-reporting-stage1`.
Existing local master was clean at `b14aa95a9954ebfc4924aed7c2088fffd96e46ea` before the branch was created.

Local original-file copies, base-file copies, local Git state, production baseline and the unmodified
production compose patch are kept outside Git at:
`C:\Users\User\Documents\Codex\backups\antispam-reporting-20261012`.
The compose patch contains an added blank line. It is preserved; neither production compose nor
the local infra checkout is overwritten. No production backup/image archive has been created by this PR.

Read-only capacity check on 12 October found 13,771,571,200 bytes available; active image size
31,002,813 bytes. This is not a deployment capacity guarantee. Recheck free space and estimate runtime
backup, image archive, build scratch and reserve immediately before any later deployment.
Do not prune images to make room automatically.

## Verification

Run focused outcome/date tests, the existing admin-forward safety replay suite, full project tests,
race checks, build, lint and comment normalization. No live Telegram messages or production DBs.

```sh
go test -short ./app/events ./app/storage
go test -race -timeout=180s ./...
go build -race ./...
golangci-lint run
```

Full storage tests require the repository's local PostgreSQL test container. Short tests exercise
SQLite. Tests cover 48-hour boundaries, unknown dates, missing acknowledgements, lost responses,
independent ban/delete failures, notification failure, partial cleanup and persisted dates after reopen.
Review existing safety replay tests for unchanged destructive action authority. A future rollout
should be separately approved and should verify sanitized API outcomes, not create real spam users.

Local Windows verification on 12 October:

- Complete `app/events` short suite, including safety replay and the new outcome tests: passed.
- Focused SQLite date/legacy/reopen tests: passed. `go build ./...`: passed.
- Comment normalization and `git diff --check`: passed. Lint of changes against the base: zero issues.
- Full short suite: the same pre-existing failures were reproduced in a separate clean base archive:
  `Test_activateServerOnly`, `Test_checkVolumeMount`, four `TestBackupDB` cases, and
  `TestSampleUpdater/with_sqlite/remove_with_timeout`. The isolated timeout case passes; it fails in
  both full runs. No tests were skipped or weakened to conceal these baseline/platform failures.
- Full Windows lint reported 116 issues versus 124 in the clean base, including existing formatting,
  long lines and detector/test warnings. Unrelated files were not reformatted or repaired.
- Windows CGO is disabled and no C compiler is installed. Linux CI separately passed reporting/safety
  replay/SQLite race tests, full project race tests including PostgreSQL, race-instrumented compilation
  and example tests. Local short tests are not a substitute for those checks.
- The PR adds `.github/workflows/moderation-reporting.yml` with explicit branch/PR triggers for those
  checks and changed-code lint. The existing full CI lint reports 47 baseline warnings; its e2e job
  fails installing `playwright-go@latest` because the upstream module declares another import path.
  These unrelated checks are not disabled or relaxed by this PR.

Raw local verification output remains with the external backup directory and is not committed.

## Rollback

Before local commit, restore only the reviewed changed tracked files from the recorded base (or
their backup), and remove only specifically named newly created PR files. Never reset the whole
workspace or overwrite unrelated work. After merge, revert this PR's commit(s).

Before any separately approved deployment:

1. Record commits, running image ID, compose hash/patch and sanitized runtime settings.
2. Save the active image and verify its archive; take consistent SQLite backup-API copies of all
   three runtime DBs and protected config/compose copies. Check archive sizes, hashes and free space.
3. Verify the old binary against a disposable copy of the additive schema. Preserve the compose diff.
4. On rollback, restore the recorded image/config through the established rollout procedure.
   Keep the additive metadata table rather than restoring an old DB unnecessarily; restoring a DB
   discards newer learning/trust state. Rollback does not reverse Telegram bans or deletions.

No deploy, container restart, Docker build or prune is part of this PR.
