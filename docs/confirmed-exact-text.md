# Confirmed exact-text rule

The detector recognizes only the normalized authored text `есть проба 0.5г` using
the existing `=...` stopword matcher. It does not prohibit `проба`, numeric weights,
or substrings. The rule is built into the engine so that a release does not depend
on importing seed dictionaries into existing runtime databases. It applies to all
instances running this engine.

Normalization follows the current stopword path: remove control/format characters,
lowercase, remove emoji, and collapse whitespace with `strings.Fields`. Control
removal happens first: a newline between words joins them rather than introducing
a space. Decimal commas, changed weights, question marks and quotation marks are
not normalized to the confirmed phrase.

Telegram passes `AuthoredText` separately from the combined detector input. The
rule checks only the author's own text even when the message replies to a channel
publication. A complaint quoting or replying to the phrase does not match this
rule. Existing classifier, general stopword and quoted-content policy is unchanged;
this exception does not guarantee that every possible complaint passes all other
detectors. Other callers that omit `AuthoredText` use `Msg`.

The rule precedes the automatic approved-user bypass, including persisted
`source=auto` records reloaded after restart. Explicit manual trust bypasses this
rule. Existing listener authorization, superuser, dry/training, journal and API
gates remain responsible for whether a detected message can be moderated.

Hidden-user forwards remain blocked before locator lookup and all mutations.
Russian feedback directs an authorized administrator to reply `/spam` to the
original in the source chat. Display names and equal text never establish identity.

## Validation

`TestDetector_ConfirmedExactText*` compares the rule with a loaded exact stopword,
checks normalization and negative phrases, and covers automatic/manual approval.
`TestConfirmedExactTextPipeline` runs the real listener, spam filter and detector
with temporary SQLite approval/journal stores and mocked Telegram APIs. It covers
persisted automatic approval, redelivery, channel identity, superusers, explicit
trust and quote/reply complaints. It records pending journal work, not API success.
`TestDetector_CASDNSRecovery` verifies bounded DNS retries, no ban verdict caused
by DNS failure, and a fresh CAS query on the next eligible message after recovery.

## Backup and rollback

Base engine commit: `b698e6cc5e08778ce94bccc71f95718ec305c136`. A complete local
Git archive of that commit and its SHA-256 were saved before editing. No production
files, dictionaries, compose configuration or runtime databases are modified by
preparing this patch.

Before a separately authorized deployment, retain the current image, configs,
compose diff, full logs and verified consistent SQLite snapshots. Roll back the
engine image to the retained image, keeping the current runtime databases and
unfinished journal work. No schema or dictionary migration is introduced here.
For a source rollback, revert this feature commit; do not reset unrelated work.
Manual moderator deletions/bans are not imported as successful bot operations.
