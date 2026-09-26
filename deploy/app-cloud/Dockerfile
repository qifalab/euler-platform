# syntax=docker/dockerfile:1
FROM golang:1.27.1-bookworm AS go-build
WORKDIR /src/services/app-cloud
COPY services/witshield-engine/ /src/services/witshield-engine/
COPY services/app-cloud/go.mod services/app-cloud/go.sum ./
RUN go mod download
COPY services/app-cloud/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app-cloud ./cmd/server
RUN cd ../witshield-engine && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/witshield-agent ./cmd/witshield-agent \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/witshield-helper ./cmd/witshield-helper

FROM node:24-bookworm-slim AS console-build
WORKDIR /src
RUN npm install --global pnpm@11.21.0
COPY platform/frontend/ ./
RUN pnpm install --frozen-lockfile
RUN pnpm --filter 'console-base^...' build \
    && pnpm --filter console-base typecheck \
    && pnpm --filter console-base build

FROM alpine:3.22 AS server
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app \
    && mkdir /data && chown 10001:10001 /data
COPY --from=go-build /out/app-cloud /usr/local/bin/app-cloud
COPY --from=go-build /out/witshield-agent /out/witshield-helper /opt/euler/device-tools/
COPY services/witshield-engine/packaging/systemd/ /opt/euler/device-tools/systemd/
USER 10001:10001
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/app-cloud"]

FROM nginx:stable-alpine AS console
COPY deploy/app-cloud/nginx.conf /etc/nginx/nginx.conf
COPY deploy/app-cloud/proxy.conf /etc/nginx/app-cloud-proxy.conf
COPY --from=console-build /src/apps/console-base/dist/ /usr/share/nginx/html/
USER nginx
EXPOSE 8080
ENTRYPOINT ["nginx", "-g", "daemon off;"]

FROM python:3.13-alpine AS maintenance
RUN addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app \
    && mkdir /data /backups && chown 10001:10001 /data /backups
COPY tools/app-cloud/sqlite_snapshot.py /usr/local/bin/sqlite_snapshot.py
USER 10001:10001
ENTRYPOINT ["python3", "/usr/local/bin/sqlite_snapshot.py"]
