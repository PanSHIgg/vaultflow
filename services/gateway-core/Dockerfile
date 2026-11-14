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
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway-core ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/gateway-core /app/gateway-core
COPY migrations /app/migrations
ENV MIGRATIONS_DIR=/app/migrations \
    PORT=8080
EXPOSE 8080
# 迁移在进程启动路径内执行 (main.go runMigrations), 无需单独 init 容器.
ENTRYPOINT ["/app/gateway-core"]
