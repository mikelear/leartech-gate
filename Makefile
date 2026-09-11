.PHONY: test-coverage vuln pre-push swag lint fetch-mk build test test-coverage

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
SWAG_VERSION := v1.16.4
GOLANGCI_BASE_URL := https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/main/go/.golangci.base.yml

swag:
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)
	@# Portable BSD/GNU sed: `-i.bak` + rm so macOS + Alpine both work.
	@sed -i.bak 's|^//	@version.*|//	@version		$(VERSION)|' cmd/server/main.go
	@rm -f cmd/server/main.go.bak
	swag init -g cmd/server/main.go -o docs

# ── Golden Go lint: delegate to the pipeline catalog ───────────────────────
#
# Runs go/leartech-go.mk from leartech-pipeline-catalog — the SAME file CI curls
# in tasks/go-lint/pullrequest.yaml — so a laptop reproduces CI byte-for-byte
# rather than approximately. Two implementations of one gate drift, and when
# they do the local one is the weaker.
LEARTECH_GO_MK_REF ?= main
LEARTECH_GO_MK_URL ?= https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/$(LEARTECH_GO_MK_REF)/go/leartech-go.mk
LEARTECH_GO_MK     := .leartech-go.mk

# Mirror the values .lighthouse/jenkins-x/test.yaml injects, so local
# `make test-coverage` and `make pre-push` reproduce what CI ENFORCES rather
# than what the golden mk defaults to.
#
# Without this, local is STRICTER than CI: the mk defaults to a 60.0 floor, CI
# injects 30.0, and a developer or agent running pre-push sees a failure that
# would never have blocked the PR. A local gate that cries wolf gets ignored
# just as fast as one that passes everything.
#
# Keep in sync with that file. Remove the override once real coverage clears
# the golden default.
COVERAGE_THRESHOLD ?= 30.0

fetch-mk: $(LEARTECH_GO_MK)   ## Fetch the golden go/leartech-go.mk from pipeline-catalog

$(LEARTECH_GO_MK):
	@echo "==> fetching $(LEARTECH_GO_MK_URL)"
	@curl -fsSL -o $@ $(LEARTECH_GO_MK_URL)

# SHELL=/bin/bash: the golden mk uses bash-only syntax. CI images ship bash as
# /bin/sh so the drift is invisible there; a laptop /bin/sh needs the override.
lint: fetch-mk   ## golangci-lint via the merged config (delegates to golden leartech-go.mk::lint)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) lint

test-coverage: fetch-mk   ## Race + coverage with the floor CI enforces (delegates to golden leartech-go.mk)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) test-coverage COVERAGE_THRESHOLD=$(COVERAGE_THRESHOLD)

vuln: fetch-mk   ## govulncheck (delegates to golden leartech-go.mk::vuln)
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) vuln

# pre-push is THE local entry point — it is what CI runs, in one command.
# `make lint` alone does NOT include govulncheck: that is a separate target,
# and running only lint is how a vulnerability finding reached a PR.
pre-push: fetch-mk   ## Full local gate: vet tidy-check build test-coverage lint vuln
	$(MAKE) SHELL=/bin/bash -f $(LEARTECH_GO_MK) pre-push COVERAGE_THRESHOLD=$(COVERAGE_THRESHOLD)

build: swag
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/server ./cmd/server

test:
	go test ./... -v -count=1 -race

