# CFNAT-AIO 多阶段构建
# 第一阶段：编译 Go 二进制
FROM golang:1.22-alpine AS builder

# CGO 需要，SQLite 驱动编译时需要
RUN apk add --no-cache gcc musl-dev

WORKDIR /src

# 复制模块定义先（利用 Docker 缓存）
COPY go.mod go.sum* ./
RUN go mod download || true

# 复制源码
COPY . .

# 静态编译（CGO 关闭也能跑的 sqlite 走纯 Go 驱动会更简单，
#   但 mattn/go-sqlite3 性能更好，这里保留 CGO 模式）
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /out/cfnat-aio ./cmd/server

# 第二阶段：最小运行时镜像
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S cfnat && adduser -S cfnat -G cfnat

COPY --from=builder /out/cfnat-aio /usr/local/bin/cfnat-aio
RUN chmod +x /usr/local/bin/cfnat-aio

USER cfnat
WORKDIR /data

EXPOSE 1234

# 默认参数可通过 docker run -e 或 compose environment 覆盖
ENTRYPOINT ["/usr/local/bin/cfnat-aio"]
CMD ["-port", "1234"]
