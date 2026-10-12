# Moderation recovery: stage 2 design amendments

Status: implemented locally in the separate stage-2 PR, opt-in and not deployed.
The stage-1 production reporting PR does not include this worker.
See [implementation specification](20261012-moderation-recovery-stage2-implementation.md).

## One receipt/action journal, not two queues

Extend the exact-source receipt design in atlas-antispam commit
`d7a24847038940166b587418738c664836c5dda1`,
`docs/architecture/ADMIN_FORWARD_IDEMPOTENCY_DESIGN.md`.
That design currently forbids automatic resume of `in_progress`/`partial` and leaves cleanup
best-effort. This stage-2 implementation supersedes that no-resume/best-effort-cleanup contract; do not layer an independent
retry queue on top of it.

Receipt identity is the validated `(gid, source_chat_id, source_message_id, identity_kind, identity_id)`.
Action rows contain independent ban/delete/cleanup states, attempt budget, deadline, next-attempt time
and owner token. Atomic receipt claim and action creation are one transaction. The worker's queue
is a query of unfinished action rows. Successful actions are never replayed because another action failed.
Automatic detection must use the same journal: the initial failures on 10 October were automatic,
not admin-forward failures. Do not backfill old incidents into automatic destructive jobs.

Repeated delivery of one admin message is deduplicated by instance/admin-chat/message ID. A new
forward has a different delivery ID but can attach to the same validated source receipt. Different
originals with identical text remain distinct. A text-locator candidate is not proof of exact source
identity. Reliable-user fallback has no exact source ID: deduplicate the same delivery, but do not
invent a source receipt from text, sender or timestamps.

## Attempt count, backoff and 24-hour horizon

The previous proposal of 50 attempts with a maximum 10-minute delay exhausts its budget in roughly
8 hours, before a 24-hour horizon. It must not be advertised as 24-hour recovery.

Proposed amendment: up to **320 total attempts per action**, 15-second request timeout, exponential
backoff starting at 2 seconds, capped at 10 minutes, with equal jitter in `[0.5 * delay, delay]`.
After the initial ramp, even the shortest scheduled delay allows fewer than 320 attempts over 24 hours;
the age deadline, rather than the count, normally ends a prolonged outage. Tests must simulate the
full schedule with controlled clocks, including jitter extremes and request durations. The attempt
cap remains a defensive ceiling and its early exhaustion is explicitly reported, never concealed.

Stop at the earliest of: attempt cap, 24 hours after journal acceptance, or for deletion
`Message.Date + 48 hours - 2 minutes`. Do not start a request that cannot fit its timeout before the
deadline. Honor 429 `retry_after` as a minimum; if it exceeds the remaining window, finalize expiry.
For unknown original dates, do not invent a 48-hour deadline: use the operation horizon and stop on
Telegram's permanent refusal. Recheck safety gates before each still-pending destructive action.

Classify transport failures, 429 and eligible 500/502/503/504; do not retry permanent refusals blindly.
429 establishes per-bot cooldown. Lost responses produce unknown outcomes: ban requires state
reconciliation; deletion replay is restricted to the same validated chat/message pair and window.
Leases do not fence an API request at Telegram. Stale workers and takeover need bounded request
lifetimes, owner-checked DB transitions and conservative reconciliation. Do not promise exactly-once.
Learning with uncertain completion must not be blindly repeated.

## Durable incomplete-moderation signal and notification delivery

Persist an incident signal and an outbox delivery row in the **same action journal transaction** that
records pending/unknown/terminal outcomes. Telegram notification failure must not lose this signal.
The outbox is a delivery view of the journal, not a second source of moderation truth. Keep structured
IDs/codes/counts only; do not persist tokens, raw messages or token-bearing error URLs.

After connectivity returns, deliver the latest incomplete-moderation summary, followed by recovery
or terminal outcome. Failure to deliver a notification never repeats a successful ban or deletion.
Notification delivery has its own bounded budget/horizon so it remains eligible after the original
delete deadline; an undelivered terminal notice stays queryable and visible operationally.

Exhausting attempts or reaching a deadline creates a durable terminal event and an outbox notice
with independent ban/delete/cleanup results, remaining message IDs and manual-action requirement.
Outbox exhaustion creates `notification_undelivered`, retained for operator inspection/export;
otherwise a total Telegram outage could make the final warning disappear.

## Prevent a notification storm

- Coalesce by instance, destination, operation and outcome generation. Repeated transport failures
  update the same pending signal, not a new notification every attempt.
- Maintain one outage summary per destination/instance and bounded periodic updates; batch counts
  during a broad outage. On recovery send one digest plus actionable terminal cases.
- Use independent delivery rate limiting, bot-wide 429 cooldown, bounded outbox and reserved capacity
  for terminal events. Never silently drop a terminal signal on queue overflow.
- If delivery acknowledgement is lost, record uncertainty; use an existing known message ID for
  edits when possible. Telegram sends have no general exactly-once guarantee, so occasional duplicates
  remain possible and must not be misrepresented as guaranteed suppression.

Tests: restart with pending and undelivered notices; lost send response; outage involving many users;
320-attempt schedules across 24 hours; deadline before retry_after; terminal moderation with failed
notice delivery; bounded queue overflow; no successful action replay; one shared receipt authority.

## Policy boundary

Stage 1 preserves current shared-admin-chat moderation and reliable-user fallback. Explicit routing,
local versus global bans, and any change to fallback authority require a separate policy task.
Do not disable fallback as a side effect of retry implementation. Stage 2 must document how fallback
delivery-scoped operations are represented without conflating them with exact-source receipts.
