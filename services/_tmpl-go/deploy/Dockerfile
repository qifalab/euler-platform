# syntax=docker/dockerfile:1.7

# Go Kratos service base Dockerfile (08§3.3 / 08§5.3).
# Multi-stage: build a static binary, ship a tiny runtime image. CI builds with
# CGO_ENABLED=0 and tags harbor.internal/cloudplatform/{service}:v{MAJOR}.{MINOR}.{PATCH}-{gitSHA}.

FROM cloudplatform/base-go1.22-build:latest AS build
WORKDIR /src
COPY . .
# golangci-lint runs in CI before this step (08§5.3).
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.Version=${GIT_SHA}" -o /out/app ./cmd/server

FROM cloudplatform/base-go1.22:latest
COPY --from=build /out/app /app/app
EXPOSE 8080
ENTRYPOINT ["/app/app"]
