# CFNAT-AIO 多阶段构建
# 第一阶段：编译 Go 二进制
FROM golang:1.22-alpine AS builder

# 支持通过 build args 传入代理，用于在受限网络环境下拉取依赖
ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG ALL_PROXY=
ARG GOPROXY=https://goproxy.cn,direct
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    ALL_PROXY=${ALL_PROXY} \
    GOPROXY=${GOPROXY}

# 安装编译和运行时所需的 CA 证书、时区数据
RUN apk add --no-cache ca-certificates tzdata

# 纯 Go SQLite 驱动，无需 CGO
WORKDIR /src

# 复制源码和 vendored 依赖（无需网络下载）
COPY . .

# 编译（再次确保 go.sum 完整，避免本地 go.sum 缺失时仍可工作）
RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o /out/cfnat-aio ./cmd/server

# 第二阶段：最小运行时镜像
FROM alpine:3.22

ARG HTTP_PROXY=
ARG HTTPS_PROXY=
ARG ALL_PROXY=
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    ALL_PROXY=${ALL_PROXY}

# 从 builder 阶段复制 CA 证书和时区数据，避免在运行时镜像中执行 apk
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

RUN addgroup -g 1000 cfnat && adduser -u 1000 -G cfnat -D cfnat

COPY --from=builder /out/cfnat-aio /usr/local/bin/cfnat-aio
RUN chmod +x /usr/local/bin/cfnat-aio

RUN mkdir -p /data && chown -R cfnat:cfnat /data

RUN apk add --no-cache su-exec

COPY scripts/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /data

EXPOSE 1234

ENTRYPOINT ["/entrypoint.sh"]
CMD ["-port", "1234"]
