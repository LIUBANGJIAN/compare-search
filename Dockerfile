# 多阶段构建：Alpine 静态编译 → 极小运行镜像
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/compare-search .

FROM alpine:3.20

# ca-certificates 供未来的 HTTPS 上游使用；tzdata 让日志时间戳可读
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /data app

COPY --from=builder /out/compare-search /usr/local/bin/compare-search

# 容器内必须监听所有网卡，否则宿主映射进来的端口连不上
# 数据文件统一放在 /data（声明为 VOLUME，便于挂载持久化）
ENV LISTEN_HOST=0.0.0.0 \
    PAGE_PORT=18899 \
    PROXY_PORT_BASE=18898 \
    COOKIE_FILE=/data/cookie_115.txt \
    CONFIG_FILE=/data/settings.json \
    NO_BROWSER=1 \
    NO_PROMPT=1

WORKDIR /data
VOLUME ["/data"]
USER 10001

# 面板端口
EXPOSE 18899
# 搜索引擎的代理端口：从 18898 往下共 8 个（每个引擎占一个）
EXPOSE 18891-18898

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PAGE_PORT:-18899}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/compare-search"]
