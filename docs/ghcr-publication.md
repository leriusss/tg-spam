# GHCR publication in this fork

DockerHub publication is disabled. No DockerHub/PAT/updater secrets are used.
The radio-t updater call has been removed; this workflow never deploys.

## Events and tags

- Automatic publication requires successful `build-app` from `.github/workflows/ci.yml`,
  a `push` in `leriusss/tg-spam`, the same head repository, master branch and no PRs.
  Checkout uses the triggering full SHA, without persisted Git credentials.
- PRs (including forks), failed builds and v* branches do not publish.
- Manual `build-only` is the default: both native architectures build and run help,
  with contents:read and no registry login/push.
- Manual `verify-publish` requires successful push CI for the exact selected SHA.
  It publishes only `verify-<run-id>-<attempt>-<full-sha>`, never master/latest.
- Automatic publication creates `sha-<full-sha>`. A separate `promote` job depends
  on `gate`, `merge`, and `pull-check` and updates master only after all succeed.
  Failed, skipped, or cancelled pull-check blocks promotion. Manual verification
  always skips promote, including after a successful pull-check.
- Promote reuses the exact manifest digest from merge that pull-check verified;
  it performs no checkout or rebuild. Immediately before the tag write, it reads
  the current master SHA through the GitHub API and skips stale source SHAs.
  A global non-cancelling concurrency group serializes the entire workflow,
  including pull-check and promotion; late old runs cannot replace newer master.
- **Release policy: latest is frozen.** Neither version branches nor manual runs
  update it. A separate reviewed release-policy change is required to enable it.

Only the publish matrix, manifest job and promotion job receive packages:write. A fresh runner
with packages:read pulls the final digest and checks both architecture configs,
OCI source/revision, and amd64 help with networking disabled and no mounts or tokens.
The binary's documented help path exits 2; the smoke check requires that expected
exit or success plus Usage output. It bypasses the data-initializing entrypoint.
OCI index annotations and image config labels identify the repository and full SHA.
Digest and immutable tag are recorded in Actions job summaries.

## Validation and activation

Run actionlint on `.github/workflows/docker.yml`. To exercise the actual JavaScript
security gate, use the command at the top of `tools/test-ghcr-gate.cjs` (Python with
PyYAML and Node required). Run the promotion dependency and scenario tests with:

```sh
python -c "import yaml,json; print(json.dumps(yaml.safe_load(open('.github/workflows/docker.yml'))))" | node tools/test-ghcr-promote.cjs
```

These tests inspect the real job graph and evaluate the actual promotion condition,
then execute the promotion script with mocked GitHub API and Docker calls. They
cover successful, failed, skipped and cancelled pull-check; failed dependencies;
stale source SHA; and manual verify-only publication. No registry write is made by
the tests. No project test or other workflow is relaxed.

The dispatch definition may need to exist on the default branch before GitHub
allows dispatch from this draft branch. If dispatch returns 422 before merge,
merge only after review, run build-only, then explicitly run verify-publish for
the same green SHA. Confirm the digest, amd64/arm64, OCI revision and pull-check
before relying on subsequent master push publication. Production migration is a
separate operation and is not authorized by this workflow.

If login succeeds but push reports permission_denied/write_package, inspect
https://github.com/users/leriusss/packages/container/tg-spam/settings : grant only
leriusss/tg-spam repository **Write** under Manage Actions access (and verify the
linked repository). Do not create a PAT or widen account permissions. A metadata
404 alone does not prove that the package is absent or identify its access policy.
Do not delete existing versions or tags to resolve permissions.
