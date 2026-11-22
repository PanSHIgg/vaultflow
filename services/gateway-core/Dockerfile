# syntax=docker/dockerfile:1
# gateway-core — multi-stage build (Go 1.26, CGO-free static binary).
# Build context: services/gateway-core
FROM golang:1.26 AS build
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=sum.golang.google.cn \
    CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
    && go build -trimpath -ldflags="-s -w" -o /out/backend ./cmd/backend \
    && go build -trimpath -ldflags="-s -w" -o /out/indexer ./cmd/indexer

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/backend /app/backend
COPY --from=build /out/indexer /app/indexer
COPY migrations /app/migrations
ENV MIGRATIONS_DIR=/app/migrations \
    PORT=8080
EXPOSE 8080
# 迁移在 gateway 进程启动路径内执行 (cmd/gateway runMigrations), 无需单独 init 容器.
# backend/indexer 经 compose entrypoint 覆盖为对应二进制.
ENTRYPOINT ["/app/gateway"]
