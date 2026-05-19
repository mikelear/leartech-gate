# leartech-gate — Claude Context

QA gate service for leartech promotion PRs. Porcupine-equivalent — Tekton presubmit on GitOps repos. Reads `leartech-qa-management` for required-tests + quill config, reads result-store for verdicts, posts PR check status + sticky comment.

> **Important: not the Go template.** Despite this repo originally being cloned from the canonical template, it is no longer it. If a session reaches this CLAUDE.md while looking for the golden Go service template to clone for a new service, **stop and clone `mikelear/leartech-go-service-template` instead** — the hub status file (`~/leartech/hub/status/leartech-go-service-template.md`) is the source of truth on "which repo is the template". Authoritative bootstrap snippet lives in `~/leartech/hub/shared-rules/multi-cluster-jx3-pattern.md` § "Bootstrap for a new service". The hub rule that governs this is `~/leartech/hub/shared-rules/repo-identity-resolution.md`.

## Hub status (loaded automatically when present)

QA gate runtime/architecture state is aggregated into `~/leartech/hub/status/qa-architecture.md` + `~/leartech/hub/status/qa-analysis.md` rather than having a dedicated `leartech-gate.md` file today. Read both before significant changes here.

## What the binaries are

- **`cmd/gate-cli/`** — the gate logic. Invoked by Tekton on each PR opened against `mikelear/jx-build-cluster-gsm` or `mikelear/jx-build-cluster-akv`. Header comment in `main.go` is the authoritative env-contract and verdict logic.
- **`cmd/server/`** — minimal HTTP stub kept because the chart expects a long-running deployable. No public API. Exposes only `/health/live`, `/health/ready`, `/openapi.json` (empty-ish), `/docs`. Not a service consumers should generate clients against — `release.yaml` deliberately does NOT run the openapi-generation task.

## Conventions in this repo

- The cmd/server stub keeps the spine of a Golden Service so `leartech-helm-library` and the catalog tasks keep working, but it is not the work — don't add real handlers there; add them to cmd/gate-cli or carve out a new cmd.
- Tests live under `cmd/gate-cli/*_test.go` and `internal/`. Coverage threshold is tracked in `.lighthouse/jenkins-x/test.yaml`.
- swag annotations stay minimal — there's nothing real to document yet, and codegen is intentionally off.
- Pipeline yaml is thin `uses:` over `mikelear/leartech-pipeline-catalog`. Do not inline pipeline logic.

## Repo layout

| Path | Purpose |
|---|---|
| `cmd/gate-cli/` | The gate CLI (the real work) |
| `cmd/server/` | Minimal HTTP stub for chart deployability |
| `internal/{config,handlers,middleware,db,tracing}/` | Standard Golden Service internals — kept up-to-date with the canonical template to maintain spine consistency |
| `migrations/` | goose SQL migrations run as Helm post-install Job (currently a placeholder) |
| `Dockerfile` | Multi-stage build for BOTH `cmd/server` AND `cmd/gate-cli` |
| `Makefile` | swag + lint + build + test targets |
| `charts/leartech-gate/` | Helm chart using `leartech-helm-library` |
| `preview/` | Per-PR preview helmfile |
| `end2end/` | Smoke + fleet-test scripts (mirror of golden template's set) |
| `.lighthouse/jenkins-x/` | Standard 11-trigger presubmit + release suite, minus codegen on release |
| `renovate.json` | Dependency bump automation |

## Pipeline triggers

Same 11-check chain as any leartech Go service — `pr`, `lint`, `test`, `govulncheck`, `security-scan`, `image-scan`, `dynamic-scan`, `ai-review`, `ai-feedback`, `end2end`, plus `release` postsubmit. Wrappers over [`mikelear/leartech-pipeline-catalog`](https://github.com/mikelear/leartech-pipeline-catalog).

## Cluster prerequisites

This repo is already registered with both clusters and cluster prereqs are satisfied — see `~/leartech/hub/CLAUDE.md` § "Registering a new repo with JX" for the reference pattern (which applies to *new* repos, not this one).

## Release mechanics

Standard multi-cluster JX3 pattern — `jx-release-version --previous-version from-tag`, cluster-suffixed tags (`v0.X.Y-gcp` / `v0.X.Y-az`), cosign-signed image, jx-promote auto-PRs. The codegen sibling task is intentionally absent from `release.yaml` — no public API surface to generate clients against. See `~/leartech/hub/shared-rules/multi-cluster-jx3-pattern.md`.

## Dependencies

- [`mikelear/leartech-pipeline-catalog`](https://github.com/mikelear/leartech-pipeline-catalog) — Tekton task catalog
- [`mikelear/leartech-qa-management`](https://github.com/mikelear/leartech-qa-management) — required-tests + quill config (Renovate-pinned)
- [`mikelear/leartech-arrivals-observer`](https://github.com/mikelear/leartech-arrivals-observer) — creates the Arrival CRs the gate reads
- `ghcr.io/mikelear/leartech-go-runtime` — build-stage base image
- `leartech-go-common`, `leartech-helm-library` — standard leartech foundations
