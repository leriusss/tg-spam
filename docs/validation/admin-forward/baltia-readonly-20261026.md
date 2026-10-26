# Baltia: read-only diagnosis, 2026-10-26

Scope: chat -1001542486391, messages 91867 and 91871. No Telegram API calls,
reclassification/replay, admission, SQL writes, container exec/restart or deploy.
Live SQLite was opened with mode=ro and PRAGMA query_only=ON. Only sanitized,
bounded metadata and allowlisted non-secret config were reported.

## Confirmed evidence

- Both originals share the same locator content hash. A read-only hash lookup in
  Baltia finds exactly two distinct originals.
- Both originals exist as one locator row each, with the same positive numeric
  user_id (not published in this report). No names or text were used as identity.
- Original Telegram dates are separately present: 1792959968 and 1792959970.
  Listener observation times are 2026-10-25 20:26:09 and 20:26:12 UTC.
- The current messages table stores hash, chat/message IDs, user_id/user_name and
  local observation time. message_dates stores Telegram date and first observation.
  No raw source update, source discriminator or update ID author provenance is
  retained for these historical rows. The old listener writes transformed From.ID,
  or SenderChat.ID when present; that code path explains the expected source of
  the stored number, but it is not per-record durable proof of these old updates.
- The author's source=auto progress lists exactly these two messages in this chat,
  count=2, with legacy global count=0. Live FIRST_MESSAGES_COUNT=3. This is below
  the trust threshold, not an established manual/trusted-author bypass.
- Current detector code advances this progress only after a non-spam verdict on
  non-short, non-checkOnly messages. Thus the durable evidence records these as
  non-spam processing. No detected_spam rows or saved per-user spam checks remain.
- Detailed checks/probabilities for those events are unavailable in the current
  container's retained logs and the inspected Baltia preflight/full logs. The
  exact detector reason/root cause cannot be proved; no present-day replay was run.
- DRY=false, TRAINING=false, TRAINING_MODE=false and MODERATION_RECOVERY=true were
  observed in the allowlisted current config. This is current configuration, not
  independent proof of every option at the original event time.
- Each original already has a separate old delete-only original_delete action:
  manual_review / delete_pending_confirmation / uncertain=false. Neither has a
  ban action. Their stored requests were pending; an expired view must render as
  expired, not authorize an operation. Nothing was confirmed or replayed here.
- Observed current container image ID:
  sha256:5a79f015bf6835f8034c3126716d887a3777ef8a20a8ceff9ccd2bde0bd6d376,
  started 2026-10-26T12:37:56.480065736Z, restart count 0. It was not changed.

## What the new flow would do (hypothetical only)

The content lookup is scoped to Baltia. A forward matching the verified shared
hash produces exactly these two candidates. The new card explicitly offers one
or both message numbers, dates and excerpts; it never selects the first row.

For these historical rows, the new migration supplies NO author provenance. A
selected original therefore permits only its exact deletion (subject to current
safety/date/deadline checks), with an explicit no-reliable-target ban refusal. A
hidden-forward name cannot repair that. An open forward with a reliable numeric
Telegram origin matching the stored row can independently provide its ban target;
this does not make the legacy locator ID itself trusted. A new original update observed after the
change can carry durable numeric author provenance and permit independent ban and
delete after selection, even when the admin forward itself hides the author.

Old delete-only requests remain delete-only. A new personal forward/selection is
recorded as a distinct explicit decision with its own history, never an automatic
upgrade of a saved request, and cannot extend an earlier deletion deadline.

The miss diagnosis is separate from the manual-flow change: this PR does not tune
classification, Han, trust thresholds, dictionaries or detector checks.
