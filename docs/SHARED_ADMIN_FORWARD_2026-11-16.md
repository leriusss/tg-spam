# One admin-forward inquiry across bot instances

Status: opt-in implementation for review. No deployment, configuration change,
incident replay, merge, or real moderation is part of this change.

## Confirmed incident and evidence limits

On 16 November 2026, read-only inspection of all five migrated containers found
the same immutable image ID
`sha256:00c045de88b87d01c4b324d6384d845dc0cb164240720d975d1b81dbc139cf7b`,
labelled source revision `251858789ac325149a88f854f164e12c22887a7a`.
The implementation is based on repository master `ac1c8fc`, which also contains
subsequently merged operator authorization and Telegram routing changes.
An image label identifies the declared source; no reproducible-build claim is made.

All five bots use the same admin chat, separate runtime SQLite databases and
separate Telegram update streams. Four admitted each forward independently:

| Bot | Local update IDs for 1324 / 1325 / 1326 | Originals | Cards | Decision at inspection |
| --- | --- | --- | --- | --- |
| Baltia | 359146321 / 359146322 / 359146323 | 92317, 92318 | 1327, 1329, 1331 | accepted; already confirmed |
| Europa | 149912235 / 149912236 / 149912237 | 20937, 20938, 20939 | 1334, 1336, 1338 | expired; none selected |
| Neoport | 318472226 / 318472227 / 318472228 | 4974, 4975 | 1328, 1330, 1332 | expired; none selected |
| Multiverse | 713871658 / 713871659 / 713871660 | 1465, 1466 | 1333, 1335, 1337 | expired; none selected |
| LES | not retained in admission tables | none | none | locator-miss logs for all three |

Every admitted delivery has its own receipt, batch and acknowledged card action.
The cause is `AcceptForward` deliberately treating a new personal forward as a
new decision epoch, independently in each process. Local deduplication of
`update_id` therefore neither coordinates bots nor coalesces new forwards.

Numeric author provenance is `telegram_update` for all nine originals. Within
each protected chat the listed originals share one author, but the four chats
have **four different numeric authors**. Names/text are not proof of common
authorship. The closed local diagnosis retains numeric IDs and source update IDs;
raw logs, user names, config values and runtime databases are excluded from Git.

Baltia's classifier marked both originals spam (53.03% spam). The worker recorded
two acknowledged original deletes, one acknowledged ban, and one ban satisfied
by `reconciled_membership`; these actions each have one recorded attempt and no
uncertain outcome. The forward batches reused those results. Europa classified
the text as 52.36% ham, Neoport as 69.99% ham and Multiverse as 97.79% ham. Their
logs record no matching stopword and similarity below threshold. CAS lookups
failed with DNS errors at the original processing times; a CAS failure was not a
positive spam verdict. Local learning/configuration differs between instances.
No incident forward authorized ban/delete in these three chats at inspection.

The journals do not retain the complete incoming Telegram payload or exact
forward-origin-to-original mapping. Thus it is **not established** which of
1324–1326 forwards the same exact original and which forwards a different
original from the series. All three found the same stored candidate sets.
LES's receipt-free misses establish processing, not its numeric update IDs.
Logs are application evidence, not a raw HTTP trace: no recorded retries were
found for the incident actions, but unseen manual Telegram operations or
historically lost logs cannot be excluded. Screenshots are not action evidence.

## Preservation before architecture changes

Free disk was checked: 9.3 GiB available, 84% used. No image build was performed
on the VPS. A new private 2.2 MB archive contains five SQLite **online backups**,
five current configs, Compose, container metadata, retained logs and exports of
all moderation tables (including unfinished work, decisions and deadlines).
Each bot snapshot is transactionally consistent; this is not a global freeze
across independent databases. All five passed `PRAGMA integrity_check`.

Member SHA256 values and the archive checksum were checked again after copying
to an NTFS directory restricted to the operator and SYSTEM outside the VPS.
Archive SHA256:
`af1137e21c967942f83b4e459e1cc90f1fc6a622fd399ad94deb8cb90e1d4337`.
Both production source and implementation-base source were archived locally.
The prior verified offsite recovery image remains available. These are backup
writes only; runtime DB contents, configurations and containers were not changed.

## Architecture and identity

A separate shared SQL database holds only roster, inquiries, bot-scoped
deliveries, search snapshots and frozen decisions. Each bot can access its own
runtime DB and the coordination DB; no bot opens another runtime DB. This is a
trusted deployment boundary: database credentials grant coordination write
access, not an untrusted multi-tenant service API.

SQLite supports a single host with the **same local file** mounted into all
containers (including its directory for locks/WAL). PostgreSQL supports separate
hosts. Never place SQLite on network storage or accidentally mount five copies.
Do not use a runtime database/schema as the shared database. The store reuses
the existing SQL transaction machinery, including SQLite immediate transactions
and the PostgreSQL control-row lock; there is no in-memory-only ownership mutex.

The incoming key is `(admin_chat_id, message_id)`. `update_id` is unique only
within a numeric bot ID. Every bot searches only its own configured primary
chat. A local immutable receipt and outbox entry commit together; zero matches
and search refusals are persisted too. Local cards for these receipts are
suppressed. Ordinary detector, reporting and other moderation paths are unchanged.

Repeated inquiries coalesce while active only when they refer to the same
exact local original IDs **and numeric provenance**, not text or display names.
The full delivery ledger remains separate. Different originals retain separate
target identities. A changed candidate set starts a new inquiry; it cannot
expand an existing confirmed selection. A provisional inquiry can be merged
only before its card send is reserved and before any decision exists.

With crossed late deliveries, different bots may initially report disjoint
original sets for two forwards. Their relation is not yet proven, so provisional
partial cards can exist. Once every peer establishes the same exact candidate
sets, reconciliation keeps one active card and edits the additional known
anchor into a short link to it, invalidating its action buttons. It never
combines two already accepted decisions. If evidence never arrives, it cannot
truthfully infer equivalence from text; the partial inquiries expire normally.

One fixed bot ID owns the Telegram card and callbacks. Other processes may
resume that bot's work after restart, using SQL claims. A different bot cannot
edit the original bot's Telegram card; owner migration requires an explicit
procedure. Collection waits two seconds and then shows partial results, with
the number of missing peers. Late reports update the same card and invalidate
stale choice buttons without expanding a saved decision.

If all configured bots answered and exactly one original is admissible without
retry review, the existing single-original workflow proceeds. Otherwise the
operator selects a chat, a particular original in Details, or all explicitly
listed admissible originals. A current admin may choose even if another admin
forwarded first. The callback actor is recorded as decision operator; original
delivery actors remain in the delivery history. The role is checked through
the actual owner's API and again by each worker before effects.

Frozen choices are imported only into the corresponding local receipt. The
existing worker alone performs ban/delete, preserving numeric user/sender_chat
checks, current admins/superusers/trust, dry/training and journal deduplication
against local detector actions. No automatic cross-chat propagation is added.

## Crash recovery and bounded resources

An absent coordinator leaves locally admitted inquiries in a persistent outbox.
It never falls back to five independently speaking bots. Opening/reconnecting
the coordination database happens in a separate background loop, so normal
automatic moderation continues. There is no event forwarding back into Telegram
updates and therefore no propagation cycle.

The loop polls every five seconds; unchanged reports and completed imports are
cached as an optimization, with SQL still authoritative after restart. Card
creation/edit claims have 30-second leases and 15-second HTTP timeouts. Known
rejected retryable sends/edits have at most five attempts with ten-second waits.
An uncertain first send, lost acknowledgement, or abandoned initial-send lease
is marked `unknown`: it is **never blindly resent**. Missing edit anchors are
not replaced with new cards. Operator reconciliation of such cases is required;
there is no exactly-once Telegram API guarantee.

Choices expire after 15 minutes; local expiry can be earlier when handoff was
delayed. A permanent refusal, expiry or finished decision requires a **new
forward plus explicit choice** for another attempt. Confirmed actions are linked
instead of repeated; earliest delete deadlines survive new receipts. Old case
callbacks remain bound to their own ref/revision/anchor and cannot authorize
the next attempt. Unknown action outcomes follow the existing worker's recovery
rules, never a second coordinator action executor.

Hard limits: five peers, 20 originals per peer, 500 inquiries in the shared DB and
500 delivered-or-pending outbox entries per bot (2,500 delivery rows total). Each report omits source quotes;
the local journal retains them. The finite outbox bounds delivery/search history
as well. At capacity, new manual inquiries fail closed; automatic moderation
keeps its existing queue and limits. Terminal history is not silently discarded.
Archive/rotation needs an explicit maintenance procedure retaining receipt,
delivery and earliest-deadline tombstones before raising the limit. This initial
bounded design is for the five-bot fleet, not an indefinite high-volume service.
Expect tens to low hundreds of MiB of metadata at the cap including delivery
snapshots, indexes and journal overhead; these are estimates, not a capacity
benchmark. Full-history JSON reads also use transient buffers. Polling is bounded,
with no build-time or continuously resident extra service process. Measure actual
SQL latency and disk growth during the future observation period.

## Card and validation

`sharedCard` is the actual renderer. Summary omits repeated source quotes and
message IDs; Details paginates exact originals, provenance, deadlines and action
outcomes. Deletion and blocking are reported separately. `Absent` never claims
that this bot deleted a message. Candidate discovery is never reported as a
completed operation. Examples in `docs/examples/shared-forward/` are generated
from the multi-instance test fixture, with synthetic identities only.

Regression tests use real separate SQL journals, the listener source recorder,
actual forward handler, actual shared renderer and actual recovery worker with
Telegram mocks. PostgreSQL exercises the same incident scenario in full CI.
Existing build/test/race/lint, recovery, reporting, auto-trust and e2e workflows
are retained without weakening checks. Final results and exact reviewed HEAD are
reported in the PR and the local validation receipt.

## Future activation, observation and rollback

1. Review and approve the exact engine HEAD independently. No activation is
   authorized by this document or draft PR.
2. Take fresh per-bot SQLite online backups, configs and shared-store backups;
   verify integrity, manifests and an offsite copy. Check disk before builds.
3. Prepare a dedicated local shared `.db` or PostgreSQL database, bounded access,
   identical numeric bot-ID roster and fixed owner. Configure
   `SHARED_FORWARD_DB`, `SHARED_FORWARD_OWNER`, `SHARED_FORWARD_PEERS` (comma
   separated), with `MODERATION_RECOVERY=true` for all participating instances.
   All values are absent by default; no Compose production values are supplied.
4. Coordinate activation across all five bots. Mixed enabled/disabled operation
   can still produce legacy independent cards and is not a supported rollout.
   Fence only admin-forward intake during the transition; do not run a second
   poller or stop unrelated services. Confirm all five share the same DB/roster.
5. Observe for **15 minutes**: owner identity, one anchor per inquiry, missing
   peers, outbox admission/import, role checks, worker outcomes, API rate limits,
   DB integrity, queue size, memory/CPU, disk and ordinary automatic moderation.
   Synthetic real moderation requires separate authorization.
6. Preserve existing incident cards and their histories. Do not import, click,
   replace, replay or auto-edit them. Legacy buttons remain subject to their
   original journals and deadlines; the new feature governs new intake only.
7. To roll back, fence new shared intake, preserve/export current shared state
   and every **current** runtime DB, and account for already imported local
   actions before disabling coordination. The simplest rollback clears the shared
   feature flag while retaining this binary and its silent-card guard. Revert to
   a pre-feature binary only after deciding how to finish or cancel still-authorized
   local work and applying the notification fence below. Keep shared
   state for review; no cross-database transaction or global cancel is promised.
   Additive SQL tables are structurally compatible with the old binary, but its
   worker does not know the silent-card table. **Never restore an
   old runtime DB**: that would lose decisions, deduplication and deadlines.

For an older-binary rollback, stop only the affected bot workers after taking the
fresh backups above. In each affected current runtime DB, run this transaction
with a bound `gid`; use `BEGIN IMMEDIATE` for SQLite, `BEGIN` for PostgreSQL:

```sql
UPDATE moderation_cards SET destination=0
WHERE gid=:gid AND EXISTS (
  SELECT 1 FROM moderation_silent_cards s
  WHERE s.gid=moderation_cards.gid AND s.receipt=moderation_cards.receipt
);
```

Commit and verify only silent card contexts changed. Preserve all receipt
destinations, decisions, actions, targets and deadlines. This makes an older
worker's possible local-card notifications fail its existing
`notification_destination_changed` gate before any Telegram call; it does not
withdraw authorized moderation actions. Known shared anchors belong to the
shared DB and remain untouched. The rollback regression checks this SQL and the
existing recovery gate using legacy-shaped notification work; it is not a
full deployment rehearsal of the old binary. Confirm this boundary in the future
staging rehearsal before deployment. No old DB restore or shared-state deletion
is part of the procedure.

No real production action, merge or deployment was performed for this PR.
