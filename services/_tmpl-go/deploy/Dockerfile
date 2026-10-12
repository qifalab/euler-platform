# syntax=docker/dockerfile:1.7

# Go service image — one Dockerfile for every service in the monorepo
# (08§3.3 / 08§5.3). Multi-stage: build static binaries, ship a distroless
# runtime image.
#
# Build context = the REPOSITORY ROOT: every service's go.mod has
# `replace github.com/qifalab/euler-platform => ../../pkg-go`, so pkg-go must
# be inside the context next to services/<svc>.
#
#   docker build -f services/_tmpl-go/deploy/Dockerfile \
#     --build-arg SERVICE=services/svc-order \
#     --build-arg MAIN=server \
#     -t harbor.internal/cloudplatform/svc-order:<tag> .
#
# Image layout (the Helm charts rely on it):
#   /app/<dir>  every main package under <SERVICE>/cmd/<dir>
#               (svc-iam: /app/web-auth, /app/verify, ...)
#   /app/app    the service's main binary = cmd/<MAIN>; the ENTRYPOINT
#               (MAIN defaults to server; svc-iam builds with MAIN=web-auth and
#               its pod runs /app/verify as a second container)
#
# CI (platform/ci-templates/app-repo-pipeline.yml) builds with kaniko,
# CGO_ENABLED=0, and tags harbor.internal/cloudplatform/{service}:<tag>.

# Every go.mod declares `go 1.26`; an older toolchain refuses to build them.
ARG GO_IMAGE=golang:1.26.6
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
# Service directory relative to the repository root, e.g. services/svc-order.
ARG SERVICE
# cmd/<MAIN> becomes /app/app.
ARG MAIN=server
ARG GIT_SHA=unknown
# Optional module proxy (e.g. a goproxy mirror); unset = Go's default.
ARG GOPROXY
WORKDIR /src
COPY pkg-go/ pkg-go/
COPY ${SERVICE}/ ${SERVICE}/
WORKDIR /src/${SERVICE}
# golangci-lint / go vet / go test run in CI before this step (08§5.3).
RUN set -eu; \
    test -n "${SERVICE}" || { echo "build-arg SERVICE (e.g. services/svc-order) is required" >&2; exit 1; }; \
    go mod download; \
    mkdir -p /out; \
    for dir in ./cmd/*/; do \
      name="$(basename "$dir")"; \
      CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.Version=${GIT_SHA}" -o "/out/${name}" "$dir"; \
    done; \
    test -x "/out/${MAIN}" || { echo "MAIN=${MAIN}: no main package at ${SERVICE}/cmd/${MAIN}" >&2; exit 1; }; \
    cp "/out/${MAIN}" /out/app

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/ /app/
# distroless :nonroot = uid/gid 65532, matching the charts' securityContext.
USER 65532:65532
ENTRYPOINT ["/app/app"]
