# Startup shutdown and events CI budget

Baseline: `96ff9b90fb41f24dcda394cbf762d3d8fe8a9f60` (PR #22 merge).
This correction changes tests and CI, fixes connection lifetime in production
`Journal.transaction`, and clears rolled-back `SharedStore.ClaimCard` results.
The production listener, moderation
decisions, SQLite settings, retry budgets and deadlines are unchanged.

## Execute shutdown contract

The SDK polling context is separate from the execute context. Calling
`StopReceivingUpdates` closes updates; this must remain an error when execute's
context is live. Canceling execute instead must return an error wrapping
`context.Canceled`, after its listener has joined the recovery worker.

The startup fixture now synchronizes shutdown in this order:

1. Observe the mock HTTP polling-start event and the confirmed recovery action.
2. Cancel execute while the SDK channel remains open.
3. Receive execute's result and assert cancellation and zero recovery workers.
4. Stop SDK polling and observe the HTTP request-release event.

A separate subprocess stops SDK polling with a live execute context. It requires
the exact unexpected-channel-close error, verifies the context is still live,
and checks worker join and polling request release. Neither path accepts a
choice of two errors. No sleeps schedule shutdown; all event waits have finite
deadlines. Existing startup retry, confirmed action/attempt, permanent failure,
secret-redaction and database/server cleanup checks remain.

Local Linux Go 1.25.14 verification: 20 race repetitions (60 child processes),
173.106 seconds, passed. Recovery CI now makes these repetitions mandatory with
a 240-second package deadline, leaving headroom above that measured run.

## Cancellation at the SQLite BEGIN boundary

An additional instrumented local run finished in 348.714 seconds but failed
`TestInstructionLateProofRetiresProvisionalCard` with "cannot start a transaction
within a transaction". This is a real connection-cleanup defect, distinct from
the original package timeout; that run is not counted as successful validation.

The cause is the gap between SQLite executing `BEGIN IMMEDIATE` and returning
its result. The vendored modernc driver's `stmt.exec` has a deferred interruption
check that can report `ctx.Err()` after execution. The journal previously assumed
an error meant BEGIN had not opened a transaction. It released the connection
without rollback or discard; a subsequent operation reused that active
transaction and failed its own BEGIN.

On a BEGIN error, the journal now marks the reserved connection `driver.ErrBadConn`
through the standard `sql.Conn.Raw` API before releasing it. Closing the physical
SQLite connection rolls back any open transaction. The original error is still
returned; there is no speculative second BEGIN, swallowed error, renewed retry
budget or acquired claim. The existing retry loop and SQL ownership boundary
are unchanged. This cleanup is necessary for correctness under listener/worker
cancellation, not an SQLite performance setting.

`TestClaimCancellationLeavesUsablePool` makes this boundary deterministic using
the existing real-file SQLite driver probe: execute real BEGIN, cancel, return
the driver's ambiguous canceled result, then reuse the same SQL handle. It
checks no owner/success on failure, unchanged attempts and control version,
released reserved connections, exactly one subsequent claim/attempt, and no
second claim. A second phase cancels after real writes but before COMMIT to
preserve the existing rollback guarantee. The BEGIN phase fails consistently
with the old journal; the COMMIT phase passes. The corrected journal passed 20
race repetitions of both phases plus all existing claim-contention targets in
49.048 seconds. These phases are also included in recovery CI's 20-repeat step.

## Shared-card ownership belongs to a committed attempt

One independent full Linux race/atomic-coverage profile exposed another real
defect: `TestSharedConcurrentCardClaimsAcrossRestartedStores` counted two
successful callers instead of one. The SQL transaction callback can run again
after SQLITE_BUSY and rollback. `ClaimCard` captured `out` and `claimed` outside
that callback, set them before COMMIT, and retained those values if a retried
attempt lost the lease to another SQL handle. A terminal COMMIT error could
also return a successful flag and an uncommitted lease.

The fix resets those two result values on every transaction attempt and returns
an empty result/false on transaction error. It does not change lease durations,
retry rules, case selection or SQL ownership. A rolled-back attempt grants no
permission to send or edit a card.

Two deterministic regressions use real file-backed SQL through the existing
driver probe. One refuses COMMIT after the real claim writes and checks there
is no successful return or persisted lease. The other refuses the first COMMIT,
observes the real ROLLBACK, lets a separate SQL handle commit its lease, and
only then lets the original call retry. It requires the retried loser to return
false/no lease, with exactly one persisted attempt and the winning handle's
lease. Both regressions fail on the old `ClaimCard`. The probe forwards native
`BeginTx` so read-only store transactions remain real as well.

Recovery CI repeats both regressions and the existing genuine concurrent
restarted-store scenario 20 times. The original concurrent assertions and their
PostgreSQL counterpart are retained. Failed intermediate profiles are retained
as evidence and are not counted toward the final candidate's stability runs.
Locally, the combined 20-repeat race run of all six claim regression targets
completed successfully in 87.533 seconds.

## Why the events budget changes

The original package-wide 180-second alarm expired while the active tests were
only 0–9 seconds old, rather than one test blocking for three minutes:
[failed merge build](https://github.com/leriusss/tg-spam/actions/runs/44180041712).
The identical reviewed tree had previously completed full race/atomic coverage
in [134.227 seconds](https://github.com/leriusss/tg-spam/actions/runs/43955134795)
and [156.512 seconds](https://github.com/leriusss/tg-spam/actions/runs/43955125678).
The latter leaves only 23.5 seconds for runner and SQL/filesystem variation.

The suite repeatedly creates real file-backed SQL fixtures and runs migrations
for isolated instances, including reopen/restart scenarios. It combines serial
SQL scenario matrices with parallel shared-coordinator tests. A local WSL
race/atomic-coverage `-short` diagnostic exceeded 300 seconds after completing
221 top-level tests; individual completed scenarios were at most 23.24 seconds.
This is a slower filesystem/environment measurement, not a successful full run
or a CI runner timing. It also shows why elapsed suite time must be measured
separately from individual test time and from overlapping parallel durations.

Two complete local Linux Go 1.25.14 baseline runs, including all four PostgreSQL
test groups, passed with race and atomic coverage in 495.068 seconds (CPU/block
profiling enabled) and 403.316 seconds (without profiling). Both used two CPUs
and a 600-second diagnostic deadline on Docker Desktop's Linux filesystem;
they are not measurements of the faster GitHub runner or the final fix.
The first profile sampled 229.80 CPU seconds: SQLite parsing accounted for
37.98% cumulatively, `NewJournal` for 20.03%, lifecycle migration for 6.29%, and
`newForwardFixtureDB` for 10.78%. These cumulative paths overlap and must not be
added. They confirm substantial real SQL/migration cost across many fixtures.
Block time is dominated by test scheduling/channel waits and idle SQL opener
goroutines; its aggregate across goroutines is not wall-clock duration.

Fixture inspection retains `t.Cleanup` SQL-handle closure and explicit reopened
handles in restart/inter-handle scenarios. Removing their migrations or changing
database pragmas would remove realistic coverage rather than fix a proven
fixture setup defect. The confirmed lifetime defect is fixed as described above.

CI keeps the complete `./...` set, race, atomic coverage, PostgreSQL and all
assertions, with a 360-second per-package deadline and a 25-minute job ceiling.
The focused and full recovery suites use the same 360-second package budget.
The existing 20 claim-contention and 10 coordinator-contention targets are
retained; the cleanup regression is added to the 20-repeat step. No fixture is
replaced with mocks or an in-memory database and no
production SQLite pragma is adjusted to make CI faster.

## Repeatable full-package diagnosis

Dispatch `moderation-recovery.yml` at the candidate branch with
`events-profile=true`. In addition to the mandatory recovery job, this runs
three independent Ubuntu jobs with Go 1.25 and `GOMAXPROCS=2`. Each runs the
entire events package, including PostgreSQL, with race and atomic coverage:

```sh
go test -json -race -count=1 -timeout=600s -covermode=atomic \
  -coverprofile=events.cov -cpuprofile=events.cpu -blockprofile=events.block \
  ./app/events | tee events.json
go tool pprof -top -cum -nodecount=25 events.cpu
go tool pprof -top -nodecount=25 events.block
```

The 600-second diagnostic limit allows collecting profiles on slower hosts; it
does not replace the mandatory 360-second build/recovery gates. Each diagnostic
job has a 15-minute ceiling and uploads JSON durations, coverage, CPU/block
profiles and their summaries for 14 days, including output on failure.
The PR validation results record the exact candidate SHA, three elapsed times,
profile interpretation, PostgreSQL results and required CI outcomes.

All traffic uses test fixtures and Telegram mocks. These checks do not exercise
production data, deployment, image downgrade or either policy rollback.
