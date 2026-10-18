# 运行时镜像：二进制由 CI 预编译并下载到 bin/ 后拼装，
# 镜像内不再拉 Go 工具链（构建从 ~10min 降到 ~1min）。
FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 qbhive

WORKDIR /app

COPY --chmod=755 bin/qbhive /app/qbhive
COPY web/static /app/web/static

RUN mkdir -p /app/data && chown -R qbhive:qbhive /app

USER qbhive
ENV QBHIVE_CONFIG=/app/data/config.json
ENV QBHIVE_WEB=/app/web/static

EXPOSE 8088

# /api/ping 在鉴权白名单里（见 internal/web/server.go 的 authMiddleware），
# 配了 QBHIVE_TOKEN 之后探活仍然通。
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8088/api/ping >/dev/null || exit 1

ENTRYPOINT ["/app/qbhive"]
