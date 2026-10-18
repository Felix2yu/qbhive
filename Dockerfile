# 运行时镜像：二进制由 CI 预编译并下载到 bin/ 后拼装。
#
# 基镜 ghcr.io/felix2yu/base:alpine-3.24 已提供 ca-certificates、tzdata、su-exec
# 与 PUID/PGID 降权入口；原先的 `USER qbhive` 由基镜入口的降权取代。
# 契约见 Felix2yu/.github/docs/docker.md。
FROM ghcr.io/felix2yu/base:alpine-3.24

WORKDIR /app

COPY --chmod=755 bin/qbhive /app/qbhive
COPY web/static /app/web/static

RUN mkdir -p /app/data

ENV QBHIVE_CONFIG=/app/data/config.json \
    QBHIVE_WEB=/app/web/static \
    APP_DATA_DIR=/app/data

EXPOSE 8088

# /api/ping 在鉴权白名单里（见 internal/web/server.go 的 authMiddleware），
# 配了 QBHIVE_TOKEN 之后探活仍然通。
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8088/api/ping >/dev/null || exit 1

CMD ["/app/qbhive"]
