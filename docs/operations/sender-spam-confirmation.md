# SenderChat /spam confirmation

Commands posted as a channel or anonymous sender cannot establish the human operator. They submit a request only. The linked channel, title, username, approved-user state and shared Telegram From IDs confer no moderation authority.

## Contract

- Only `/spam` in the configured primary group enters this path. It must reply to an original in that same group. A numeric ordinary-user identity (including deleted accounts) or a distinct negative SenderChat identity is required. Anonymous group targets, bot/shared identities, missing originals and explicit protected targets are refused.
- A channel, including an unrelated channel, can request review; it cannot authorize it. The request stores the exact source chat/message/identity and original text in the existing recovery card. Hidden forwards keep their existing rejection gate.
- The existing recovery journal owns all actions. Before approval, original/cleanup actions are manual placeholders. No learning, trust change, ban or deletion occurs. Cleanup selection is scoped to the primary chat; no legacy unscoped fallback is used.
- Existing M callbacks and cards are reused. A callback must be bound to the persisted card destination/message/reference. Only a non-bot, positive, non-shared From ID in the existing **numeric** superuser list can decide a channel request. Group administrators are loaded as numeric IDs by the existing startup mechanism. Username-only superusers retain existing personal-command behavior but cannot approve this path until their numeric identity is configured through a separately approved operational change.
- The SQL transaction checks the 15-minute deadline and claims one decision per source request. Repeat callbacks, different callback IDs and restart cannot repeat learning or confirmed actions. Learning or trust-update failure leaves the decision incomplete for manual recovery; it does not announce success or replay learning automatically. Existing worker authorization holds API actions until decision completion.
- Three new requests per sender per minute, twenty per instance per minute, plus existing journal retention/capacity limits. Feedback is limited to one reply per sender per minute and twenty per instance; the throttle persists across restart. Repeated commands against the same source coalesce. A request that expires or is rejected is not silently renewed by replay; the administrator must resolve it explicitly. Expiry prevents approval even if a stale Telegram keyboard remains visible. Expired/rejected cards offer explicit closure without moderation; rejected manual placeholders are canceled.
- Dry/training reject this channel-confirmation path without learning or moderation. Personal superuser commands and other report policies remain unchanged.

## Messages

Discussion: `Команда /spam принята. Запрос ожидает подтверждения в админ-чате (15 минут). До подтверждения модерация и обучение не выполняются.`

No target: `Не удалось выполнить /spam: Telegram не передал надёжный числовой ID автора оригинала. Модерация и обучение не выполнялись.`

Admin card includes source text, source chat ID, original message ID, target identity kind/ID and confirmation deadline. Buttons: `Подтвердить модерацию`, `Отклонить запрос`, `Подробнее`.

After approval: `Решение записано. Карточка обновится после выполнения`. Ban, original delete and cleanup retain independent actual outcomes from the worker. Approval is not proof that Telegram performed the operations.

## Validation

`TestSenderSpam*`: production-shaped SenderChat routing, unrelated channel and anonymous sender as requesters, personal-superuser routing, deleted numeric target, missing/shared/anonymous/protected/cross-chat targets, dry/training, original text learning, callback identity/card/chat binding, forged display text, duplicates, rejection and alternate callback bypasses.

`TestPendingRequest*`: expiry, cancellation, sender/global rate limits, concurrent decisions, reopen with pending authority and feedback throttle, reopen after confirmed outcomes without replay.

Existing `app/events` and `app/moderation` regression suites cover hidden-forward guards, personal direct commands, independent typed Telegram outcomes, recovery, learning failures and journal ownership. Run them with race on Linux; full CI also exercises PostgreSQL.

## Release gate and rollback

This change has not been deployed. No production settings, dictionaries, runtime databases, compose files or Telegram messages are changed by its preparation. No classifier/trust thresholds or channel superuser entries are introduced.

Before an independently authorized deployment:

1. Record the currently running commit/image, container state, config/compose hashes and existing compose diff. Preserve current image, private logs/configs and consistent online backups of each current SQLite database. Check integrity and rollback readability on copies. Do not replace live DBs with fixtures or snapshots.
2. Review the exact candidate HEAD and completed CI, including Linux race/PostgreSQL. Build the approved release once. Preserve the old image. Use the established operations rollout procedure and only approved `docker-compose down` / `docker-compose up -d` switching.
3. No new configuration permission is required: existing numeric superusers are decision makers. Keep `MODERATION_RECOVERY` and admin-chat availability as explicit prerequisites. Startup health alone does not prove a live moderation outcome. Observe natural traffic; do not send tests, ban, delete or train without separate authorization.
4. Before rollback, export unfinished actions, decisions and request records; preserve **current** runtime databases including WAL/SHM consistency. Older binaries do not understand request expiry/authority metadata. Do not run an older binary with recovery enabled while new request cards can still be acted upon. Disable recovery through a separately authorized rollback configuration, preserve/export pending work, then switch to the preserved image using the established procedure. Old M callbacks must remain inert while recovery is disabled; check this on a copied fixture before release approval. Restore config/compose only from the reviewed diff, retaining pre-existing changes.
5. Never restore pre-release database snapshots as the normal rollback: that loses newly received runtime data. Keep additive request tables for forward recovery; do not drop them or overwrite current databases.

## Evidence limitations for the incident

Both source-chat `/spam` commands were delivered and entered ordinary spam filtering, which returned `pre-approved`. Production routing only calls the direct command handler after From passes the superuser gate; the observed shared channel From ID did not. Separate hidden-forward events were blocked independently.

Historical logs/locator do not retain reply-to IDs or complete raw updates. Thus the exact reply-to pairing and full original identity fields cannot be reconstructed. Candidate source messages are dated the previous day in locator/message_dates. Do not label their association as proven. No moderation receipt or sample write was found in the command window; Telegram has no separate outbound audit trail here, so absence of API calls is supported by the observed code path rather than an exhaustive API trace.
