# leartech-gate

QA gate service for leartech promotion PRs. Porcupine-equivalent — runs as a Tekton presubmit on GitOps repos (`jx-build-cluster-gsm`, `jx-build-cluster-akv`), reads required-tests + quill config from `leartech-qa-management`, verdicts from result-store, posts PR check status + sticky comment.

> **Not the Go service template.** This repo was originally cloned from the golden template, but its role evolved. If you're bootstrapping a new Go service, the canonical template is [`mikelear/leartech-go-service-template`](https://github.com/mikelear/leartech-go-service-template) — its hub status (`~/leartech/hub/status/leartech-go-service-template.md`) names it as the catalog canary. See `~/leartech/hub/shared-rules/repo-identity-resolution.md` for why.

## What this service does

- **`cmd/gate-cli/`** — Go binary invoked by a Tekton task on every PR opened against a `jx-build-cluster-*` GitOps repo. Reads the helmfile mutated by auto-promotion (one or more service version bumps), evaluates each release against the post-deploy quill (reads `Arrival` CRs in `jx-staging`), aggregates verdict, posts a PR check status + sticky comment via the GitHub API. Exits 0 (pass) / 1 (fail) so Lighthouse picks up the check.
- **`cmd/server/`** — minimal HTTP stub retained for chart deployability (`/health/live`, `/health/ready`). Not a public-API service; the work is all in the gate CLI.

Single quill today: **post-deploy-tests** (Arrival.phase=Passed contract). Shift-left-tests quill was removed 2026-05-14 — release-time test execution reinvented K8s readiness probes + post-deploy coverage without genuine value-add.

Future quills under consideration (see `~/leartech/qa-architecture/gate.md`):
- **copromotion** — pure helmfile diff check, e.g. auth-ui + auth-service must promote together for OAuth handshake compat
- **migrations** — K8s-native Helm hooks largely cover this, low value

## Design context — read first

The QA gate is part of a wider QA stack. Architecture docs in the hub:

- `~/leartech/hub/status/qa-architecture.md` — forward design + phased build plan
- `~/leartech/hub/status/qa-analysis.md` — mqube QA + release-gating reference model
- `~/leartech/qa-architecture/` — design repo with detailed gate / quill design

Related runtime repos (the gate consumes their contracts):

- `mikelear/leartech-qa-management` — single source of truth for required-tests, gate metadata, repo-type policy, notification config (consumed via Renovate-pinned tags)
- `mikelear/leartech-arrivals-observer` — Fat-Controller-equivalent — watches K8s ReplicaSets, creates Arrival CRs, dispatches post-deploy tests
- `mikelear/leartech-forensics-runner` — Tempo span-diff forensics runner dispatched on Failed Arrivals
- `mikelear/leartech-qa-sandbox-gitops` — sandbox GitOps repo for validating gate changes without disturbing real GitOps repos

## Local development

```bash
# Regenerate OpenAPI spec for the cmd/server stub after annotation changes
make swag

# Lint (fetches leartech-pipeline-catalog base + merges local overrides)
make lint

# Build the CLI + stub
make build
./bin/gate-cli   # needs HELMFILE_PATH, RESULT_STORE_BUCKET, GITHUB_TOKEN, etc. — see cmd/gate-cli/main.go env contract

# Tests
make test
make test-coverage
```

The CLI binary expects the Tekton-task environment contract documented in `cmd/gate-cli/main.go`:

```
HELMFILE_PATH       — path to the helmfile to inspect
RESULT_STORE_BUCKET — GCS bucket name (e.g. test-artifacts-product-first)
RESULT_STORE_PREFIX — GCS path prefix (e.g. results/v1/)
CLUSTER_TAG         — gcp / az
GITHUB_TOKEN        — for PR check + comment posting
REPO_OWNER, REPO_NAME, PULL_NUMBER, PULL_PULL_SHA — Tekton injects these
GCS_KEY_FILE        — path to GCS service-account key (mounted from secret)
```

## Release mechanics

Per the standard multi-cluster JX3 pattern — see `~/leartech/hub/shared-rules/multi-cluster-jx3-pattern.md`. Cluster-suffixed tags (`v0.X.Y-gcp` / `v0.X.Y-az`), cosign-signed image, jx-promote auto-PRs against each cluster's GitOps repo. Release does NOT run the openapi-generation codegen task — the gate has no public HTTP API worth generating clients for.

## References

- `~/leartech/hub/shared-rules/repo-identity-resolution.md` — why this README's previous "Golden Go service template" framing was wrong, and the rule for resolving repo roles via hub status
- `~/leartech/hub/shared-rules/golden-service-standard.md` — the contract this service satisfies as a leartech Go service
- `~/leartech/hub/shared-rules/conventions.md` — CI / pipeline rules
- `~/leartech/leartech-go-common` — auth middleware, logger, httptools
- `~/leartech/leartech-helm-library` — chart spine
