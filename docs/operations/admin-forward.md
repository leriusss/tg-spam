# Manual moderation by personal forward

## Runtime path and authorization

`TelegramListener.Do` routes admin chat messages to `admin.MsgHandler`. Only a
forward sent by a non-bot personal user with an explicitly authorized numeric ID
can create work. SenderChat/technical-user identities, plain text, service messages
and bot cards are not moderation instructions. Username-only authorization is not
sufficient. Disabled forwarding, missing journal, dry/training and admission errors
produce explicit refusals, not synchronous Telegram mutations.

Each bot calls `DeleteCandidates` only for its configured primary chat. No first
match is selected from a duplicate set. Search matches are not identity evidence.

`message_authors` is additive locator metadata, recorded by the listener exclusively
from the original Telegram update: chat/message IDs, source `telegram_update`,
update ID, identity kind and numeric User.ID or SenderChat.ID. SenderChat takes
precedence; shared technical user IDs are excluded. Conflicting observations
permanently invalidate provenance. Legacy messages are NOT backfilled from their
user_id, display names, forwarded names, or matching text. The table contains
extracted original-update evidence, not a raw Telegram payload. Message dates are
kept separately and never invented from processing timestamps.

## One decision and one persistent card

A single original is the explicit decision already: AcceptForward atomically
admits independent original_delete and policy ban/restrict actions. Unknown authors
admit only the exact deletion with a visible explanation. Known superusers, explicit
manual trust and anonymous own-chat posts are conservatively vetoed. The executor
also rechecks the saved numeric operator, current target membership/admin/superuser
status, dry/training/trust/chat gates immediately before each API call.

Multiple candidates produce one immutable 15-minute selection card. It enumerates
the exact source numbers, original dates and inert text excerpts; buttons select
one exact original or all explicitly listed originals. Callback authority is bound
to the persisted card destination, notification ID, random ref, saved numeric
requester, expiry and bounded snapshot indices. Telegram card text and display
names are never parsed as authority. Other callback verbs cannot add learning,
cleanup, bans, or renewals to these cards.

`moderation_forward_batches` is journal authority metadata, not a second execution
queue. Decisions and operation insertion share the existing SQL transaction/control
lock. All work executes through the existing Journal -> Worker -> recovery executor.
Each unique author in a selected batch gets one membership operation. Confirmed
membership facts across overlapping forward batches are reused unless a later undo
exists; confirmed original deletions are also reused by the existing exact-pair
contract. Unknown API results retain uncertainty and retry/manual states.

The batch key is a hash of primary chat plus the sorted exact original set (not the
forward delivery or operator). Re-forwarding the same set, callbacks and restarts
cannot create another decision, slide the selection expiry, renew completed work,
or extend delete deadlines. Deletion deadlines are clamped to the first recorded
batch deadline, existing actions and Telegram message age. A different explicit
selection creates its own durable history; existing delete-only receipts are never
upgraded or rewritten and their deadlines are still respected.

A result card says `Принято: удалить сообщение №… и заблокировать автора`, then
shows separate deletion and blocking states. Unknown authors explicitly show no
reliable ban target. Result pages contain at most five originals; page navigation
reads the journal without modifying decisions, deadlines or permissions. The worker
edits the same notification (coalesced at the existing one-minute notification
limit). New forward work does not emit additional generic digest notifications.
Pending choices are rendered expired after their deadline, with no action buttons;
worker expiry refreshes the persisted card without admitting any moderation.

No spam/ham learning, approved-user removal, mass cleanup or protected-chat /spam
is required by this path. Existing source-chat commands remain available as separate
explicit operations.

## Shared admin-chat noise

A bot with no candidates logs `admin forward search: outcome=locator_miss`, without
sending an identical refusal alongside other bots. To get a useful read-only miss
result, an authorized user replies to the forward with
`/forward_status@ExactBotUsername`. Only the addressed bot responds with its own
chat-scoped search result and existing allowed alternatives. The diagnostic command
never admits or replays work. No cross-bot coordinator or new service is introduced.
Genuine storage errors and safety refusals remain visible.

## Tests and review artifacts

`app/events/admin_forward_flow_test.go` exercises the real listener, durable SQL
journal, real worker/executor and renderer with Telegram mocks: unique/multiple
originals, same/different authors, hidden forwards with original provenance,
unknown/legacy authors and misses, forged callbacks, authorization and safety,
partial success/uncertainty, deadlines, concurrent decisions, restart, overlapping
SenderChat membership, no learning/cleanup, same-card edits and read-only pages.
`app/storage/locator_authors_test.go` verifies no migration backfill, scope, evidence
validation, conflicts and reopening. Existing old-forward fixtures use a test-only
historical handler to continue validating pre-existing delete-only/identity history;
that handler is not compiled into production. New runtime routing is tested through
TelegramListener, not that oracle. No test assertion or workflow is disabled.

Generate the synthetic renderer examples (no Telegram/network) with:

```sh
FORWARD_CARD_EXAMPLES=docs/validation/admin-forward go test -short -run '^TestAdminForwardRendererExamples$' ./app/events
```

## Review-only rollback plan with current databases

No deployment or rollback is authorized in this PR. For a separately approved
future rollout: record the actual pre-release image digest/config and take consistent
SQLite backups (including all current runtime state) plus sanitized counts/quick_check.
Keep a private export of unfinished moderation receipts/actions/demands/decisions,
requests, cards/bindings and forward batch snapshots. Preserve message_authors too.

If rolling back later, export unfinished work from the THEN-current DB first,
disable recovery and manual-forward admission in the approved previous image/config,
and restore only that image/config. Keep the current live DBs and all additive tables;
do not restore an old SQLite snapshot, drop tables, re-enqueue a choice, or clear
history. Old binaries cannot interpret forward batch selection snapshots and must
not run recovery against them. Reconcile pending/uncertain work explicitly before
re-enabling recovery in a compatible binary. This retains approvals, newer messages,
fixed deadlines and already confirmed API facts. Any restart/compose operation needs
its own separate production authorization.

The 2026-10-26 read-only Baltia audit observed existing delete-only pending requests
and a live database without message_authors. The additive migration does not alter
those historical requests or grant them ban authority. See the separate incident
note for evidence and remaining limits.
