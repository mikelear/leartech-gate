# Golden Dockerfile — distroless/static runtime, non-root, no shell.
# Build stage uses leartech-go-runtime (cosign-signed, weekly rebuild)
# which pre-installs git, make, ca-certificates, tzdata, and swag.
# Version pinned: Renovate bumps it on each new leartech-go-runtime release.

# ---- build stage ----
FROM ghcr.io/mikelear/leartech-go-runtime:0.28.0 AS build

# Dependency layer — cached unless go.mod/go.sum change
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The OpenAPI spec is NOT regenerated here. docs/ (including docs.go, which
# main.go imports) is committed, and `swag-check` on the lint path fails the PR
# if it drifts from the annotations — so the spec that ships is the one that was
# reviewed.
#
# Regenerating at image-build time was actively harmful: it depended on whichever
# swag the builder image happened to carry, which is the same class of bug that
# made a stale v1.8.4 silently drop an enum and a security description from
# plan-api's spec. It also broke outright on 2026-09-11 once `make swag`
# delegated to the golden leartech-go.mk, because leartech-go-runtime ships make
# and swag but no curl, so fetching the mk exited 127 and the container build
# failed with "error building stage: exit status 2".

# VERSION is baked into main.version for the /health/live payload.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/server \
    ./cmd/server

# leartech-gate is a CLI invoked from a Tekton task — build that binary too.
# (Bootstrap gap #6 from Session 0c: golden Dockerfile only builds cmd/server;
#  new sub-commands need explicit additions here.)
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/gate-cli \
    ./cmd/gate-cli

# ---- runtime stage ----
# distroless/static:nonroot — no shell, no package manager, uid 65532
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server
COPY --from=build /out/gate-cli /gate-cli
# --chown=65532:65532 makes the generated OpenAPI spec readable under any
# kernel/filesystem policy that double-checks ownership beyond the 0644
# world-read bits (some nodes reject root-owned files in distroless/nonroot
# containers with 403 via http.ServeFile).
COPY --chown=65532:65532 --from=build /src/docs /docs

EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/server"]
