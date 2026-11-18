# Startup shutdown and events CI budget

Baseline: `96ff9b90fb41f24dcda394cbf762d3d8fe8a9f60` (PR #22 merge).
This correction changes tests and CI only. Production listener, moderation,
SQLite settings, retries and deadlines are unchanged.

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

CI keeps the complete `./...` set, race, atomic coverage, PostgreSQL and all
assertions, with a 360-second per-package deadline and a 25-minute job ceiling.
The focused and full recovery suites use the same 360-second package budget.
The existing 20 claim-contention and 10 coordinator-contention repetitions are
unchanged. No fixture is replaced with mocks or an in-memory database and no
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
