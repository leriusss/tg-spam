# Bounded SQLite retry before an atomic action claim

Base: master merge `676417de73ec598beea3eadab032bf495bbdefda` (PR #20).
Production activation remains blocked pending review, merge, and successful
CI/GHCR for a new merge SHA. This PR does not access or change production.

## Failure and scope

The [merge build](https://github.com/leriusss/tg-spam/actions/runs/43919050023/job/133481049382)
failed `TestSimultaneousActionClaimsAcrossSQLHandles` with SQLITE_BUSY.
`Journal.Claim` first checks for due work using a read-only SELECT. This query
was outside the transaction's lock retry and could return a transient SQLite
lock error immediately. A separate failure path could return an in-memory owner
and `ok=true` when the transaction later rolled back.

The fix preserves the exact availability query and the atomic SQL claim. Only
typed SQLite BUSY/LOCKED results (including extended codes) from the preliminary
read retry, with a cancellable 25 ms wait. Permanent errors return immediately,
even when their text contains "locked". PostgreSQL read errors do not retry.

One context deadline is set at entry to Claim: five seconds, or an earlier
caller deadline. Read retries and the existing transaction use that exact
deadline. The transaction's existing child timeout cannot extend its parent.
Existing cancellation-independent rollback cleanup (up to one second) remains
unchanged; it grants no authority to execute an API action. No other transaction
policy, SQLite engine setting, moderation attempts/deadlines or Telegram retry
behavior changes.

An idle worker still performs only the preliminary SELECT: no BEGIN IMMEDIATE,
control-row update or other write. A positive count is only an optimization hint;
the existing SQL transaction rechecks state and commits ownership atomically.
Any claim error returns a zero Action and `ok=false`, including COMMIT failure.
There is no new mutex or in-memory claim authority.

## Deterministic proof

The test-only connector delegates to the real SQLite driver. Its observer uses
channel handshakes to identify the preliminary SELECT's actual result without
injecting a fake BUSY error or relying on sleep-based lock timing.

- `TestClaimReadBusyRecovers`: another SQL handle holds BEGIN EXCLUSIVE. The
  first real SQLITE_BUSY is observed, the lock is released before returning the
  error, and Claim must retry and commit exactly one attempt. This test was run
  against the unchanged base and failed with SQLITE_BUSY; it passes with the fix.
- `TestClaimReadDeadlineLeavesNoClaim`: the exclusive lock remains through the
  caller deadline. The result has no owner/claim and the complete stored Action,
  including attempts and its original deadline, is unchanged.
- `TestClaimReadCancellationInterruptsWait`: cancel after observing an actual
  locked read; no claim or persisted attempt is created.
- `TestClaimReadPermanentErrorIsNotRetried`: a real invalid SQL view references
  `missing_locked_column`. Exactly one read occurs and no write is attempted.
- `TestClaimSharesReadAndTransactionBudget`: after the first BUSY, a RESERVED
  lock permits the next read but blocks BEGIN IMMEDIATE. The observer checks
  exact equality of deadlines across both reads and the transaction, with both
  the default budget and an earlier caller deadline. Cancellation creates no claim.
- `TestClaimIdleReadDoesNotWrite`: three idle polls produce no Exec calls,
  no total_changes increment and no database-file hash change.
- `TestClaimFailedCommitReturnsNoOwner`: an injected permanent COMMIT failure
  rolls back actual SQL changes. The base incorrectly returns `ok=true`; the
  fix returns no owner and leaves the action pending with zero attempts.

The existing eight-contender/two-handle SQLite ownership test is retained with
its assertions unchanged. The PostgreSQL journal test now also checks eight
claims through two handles, exactly one persisted owner/attempt, and idle polls
without control-row writes. The recovery workflow adds a bounded **20-run race
series** for the original concurrent test, real read-lock recovery and failed
COMMIT ownership. Existing checks, assertions and timeouts are unchanged.

```sh
go test -short ./app/moderation -run '^TestClaim|^TestSimultaneousActionClaimsAcrossSQLHandles$'
go test -race -count=20 -timeout=180s ./app/moderation \
  -run '^(TestSimultaneousActionClaimsAcrossSQLHandles|TestClaimReadBusyRecovers|TestClaimFailedCommitReturnsNoOwner)$'
go test -race -timeout=300s ./...
```

The exact reviewed HEAD and final CI links are recorded in the draft PR. Native
Windows focused tests supplement full Linux CI, including actual PostgreSQL.

## Release boundary

No merge, release-image publication or deployment belongs to this fix before
review. The five-bot preservation archive and successful actual-old-image
rehearsal from the deployment task remain intact. After approval, verify the new
merge SHA's CI and GHCR digest, repeat affected image/copy checks on that candidate,
refresh changed runtime/shared snapshots immediately before switching, then
follow the agreed five-instance rollout and 15-minute observation schedule.
