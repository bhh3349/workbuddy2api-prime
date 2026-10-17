# 多阶段构建：编译 + 最小运行镜像
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/workbuddy2api-prime ./cmd/server

FROM alpine:3.20
RUN adduser -D -s /sbin/nologin wbprime \
    && apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=builder /out/workbuddy2api-prime /app/workbuddy2api-prime
RUN mkdir -p /app/auths /app/data && chown -R wbprime:wbprime /app
USER wbprime
EXPOSE 8788
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
    CMD wget -qO- http://127.0.0.1:8788/healthz | grep -q ok || exit 1
ENTRYPOINT ["/app/workbuddy2api-prime"]
