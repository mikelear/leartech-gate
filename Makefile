.PHONY: swag lint lint-config build test test-coverage

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
SWAG_VERSION := v1.16.4
GOLANGCI_BASE_URL := https://raw.githubusercontent.com/mikelear/leartech-pipeline-catalog/main/go/.golangci.base.yml

swag:
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)
	@# Portable BSD/GNU sed: `-i.bak` + rm so macOS + Alpine both work.
	@sed -i.bak 's|^//	@version.*|//	@version		$(VERSION)|' cmd/server/main.go
	@rm -f cmd/server/main.go.bak
	swag init -g cmd/server/main.go -o docs

lint-config:
	@command -v yq >/dev/null || { echo "yq required (brew install yq)"; exit 1; }
	@curl -fsSL -o .golangci.base.yml $(GOLANGCI_BASE_URL)
	@yq eval-all '. as $$item ireduce ({}; . *+ $$item)' .golangci.base.yml .golangci.yml > .golangci.merged.yml

lint: lint-config
	golangci-lint run --config .golangci.merged.yml ./...

build: swag
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/server ./cmd/server

test:
	go test ./... -v -count=1 -race

test-coverage:
	go test ./... -v -count=1 -race -coverprofile=cover.out
	@TOTAL=$$(go tool cover -func=cover.out | awk '/^total:/ {print $$3}' | sed 's/%//'); \
	THRESHOLD=60.0; \
	echo "coverage: $$TOTAL% (threshold: $$THRESHOLD%)"; \
	awk -v t="$$TOTAL" -v th="$$THRESHOLD" 'BEGIN { exit !(t < th) }' && echo "FAIL: below threshold" && exit 1 || true; \
	echo "PASS"
