# ---- build stage ----
FROM golang:1.27-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates

# 利用 Docker layer 缓存：只有依赖变更才重新跑 go mod download
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/qbhive ./cmd/server

# ---- runtime ----
FROM alpine:3.24

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 qbhive

WORKDIR /app

COPY --from=builder /out/qbhive /app/qbhive
COPY web/static /app/web/static

RUN mkdir -p /app/data && chown -R qbhive:qbhive /app

USER qbhive
ENV QBHIVE_CONFIG=/app/data/config.json
ENV QBHIVE_WEB=/app/web/static

EXPOSE 8088

ENTRYPOINT ["/app/qbhive"]
