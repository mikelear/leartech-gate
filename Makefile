.PHONY: swag swag-check lint fetch-mk build test test-coverage vet tidy-check vuln secrets pre-push

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GOLANGCI_BASE_URL := https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/main/go/.golangci.base.yml


# ── Golden Go lint: delegate to the pipeline catalog ───────────────────────
#
# Runs go/leartech-go.mk from leartech-pipeline-catalog — the SAME file CI curls
# in tasks/go-lint/pullrequest.yaml — so a laptop reproduces CI byte-for-byte
# rather than approximately. Two implementations of one gate drift, and when
# they do the local one is the weaker.
LEARTECH_GO_MK_REF ?= main
LEARTECH_GO_MK_URL ?= https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/$(LEARTECH_GO_MK_REF)/go/leartech-go.mk
LEARTECH_GO_MK     := .leartech-go.mk

fetch-mk: $(LEARTECH_GO_MK)   ## Fetch the golden go/leartech-go.mk from pipeline-catalog

$(LEARTECH_GO_MK):
	@echo "==> fetching $(LEARTECH_GO_MK_URL)"
	@curl -fsSL -o $@ $(LEARTECH_GO_MK_URL)

# SHELL=/bin/bash: the golden mk uses bash-only syntax. CI images ship bash as
# /bin/sh so the drift is invisible there; a laptop /bin/sh needs the override.
lint: fetch-mk   ## golangci-lint via the merged config (delegates to golden leartech-go.mk::lint)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) lint

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/server ./cmd/server

test: fetch-mk   ## Unit tests (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) test

# COVERAGE_THRESHOLD mirrors .lighthouse/jenkins-x/test.yaml EXACTLY. The whole
# point of delegating is that a laptop reproduces CI, and a local floor stricter
# than CI's is just as much a divergence as a looser one — it makes `make
# pre-push` fail on a tree CI would pass, and the habit that forms is ignoring
# it.
#
# The 30.0 is a leftover: it arrived with the comment "Template has no business
# logic yet", copied from leartech-go-service-template when this repo was
# cloned from it. leartech-gate is the QA gate service and has had real logic
# for a long time. Raising it to the 60.0 golden standard is tracked separately
# — it needs tests, not a number change, and hiding that behind a silently
# looser local gate is how it stayed at 30 this long.
COVERAGE_THRESHOLD ?= 30.0

test-coverage: fetch-mk   ## Race + coverage with the floor CI enforces (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) COVERAGE_THRESHOLD=$(COVERAGE_THRESHOLD) test-coverage

vet: fetch-mk   ## go vet (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) vet

tidy-check: fetch-mk   ## Verify go.mod/go.sum are tidy (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) tidy-check

vuln: fetch-mk   ## govulncheck (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) vuln

secrets: fetch-mk   ## gitleaks (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) secrets

# pre-push is THE local entry point — what CI runs, in one command.
#
# This repo had no pre-push target at all, and no vet/tidy-check/vuln/secrets
# either. `make lint` alone does NOT include govulncheck: that is a separate
# target, and running only lint is how a govulncheck finding reached a PR here
# on 2026-09-11 with nothing local to catch it first.
pre-push: fetch-mk   ## Full local gate: vet tidy-check build test-coverage lint vuln
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) COVERAGE_THRESHOLD=$(COVERAGE_THRESHOLD) pre-push

# swag/swag-check used to be implemented here. The local recipe installed the
# pinned SWAG_VERSION only `if ! command -v swag` — so whichever swag a laptop
# already had won, and a different minor version emits a different spec. On
# 2026-09-11 that made leartech-plan-api report docs/swagger.json "not in sync"
# against a spec the golden check confirms is correct, and regenerating with a
# stale v1.8.4 silently dropped an enum and the bearer-token security
# description. release.yaml publishes five SDKs from that spec.
#
# The constant is gone too: SWAG_VERSION here had drifted to v1.16.4 against
# go.mod's v1.16.6. A second source of truth for a version is a second thing to
# forget, so the golden mk reads go.mod, reinstalls on MISMATCH rather than
# absence, and renders to a temp dir instead of overwriting docs/.
swag: fetch-mk   ## Regenerate docs/ from annotations (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) swag

swag-check: fetch-mk   ## Fail if docs/ is stale (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) swag-check
