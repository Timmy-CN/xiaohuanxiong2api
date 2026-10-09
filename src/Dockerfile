# xhx2api 多阶段构建（SPEC2 §C.1）。
#
# 构建层：golang:1.24-bookworm；纯静态二进制（CGO_ENABLED=0），
# 用 -ldflags 注入版本号；-buildvcs=false 因为构建上下文里没有 .git。
FROM golang:1.24-bookworm AS build

ARG VERSION=0.2.0
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X xhx2api/internal/version.Version=${VERSION}" \
    -o /out/xhx2api ./cmd/xhx2api

# 运行层：debian:bookworm-slim，只带二进制与运行时依赖（CA 证书、时区数据）。
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -r -u 10001 -m app
WORKDIR /app
COPY --from=build /out/xhx2api /app/xhx2api

EXPOSE 3066
USER app

# 健康检查：复用二进制的 -healthcheck 模式（发一次 GET，2xx 退出 0）。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/xhx2api","-healthcheck","http://127.0.0.1:3066/healthz"]

ENTRYPOINT ["/app/xhx2api","-config","/app/config.json"]
