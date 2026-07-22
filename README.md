# CFNAT-AIO

**All-In-One Cloudflare CMIN2 优选 IP 工具** — 整合 cfnat 代理转发、cfdata IP 扫描与 WebUI 控制台于单一容器进程。

## 特性

- **多地区代理**：每个地区独立 SOCKS5/HTTP/WebSocket 端口，WebUI 动态增删改，配置变更无需重启
- **多协议支持**：支持 SOCKS5、HTTP CONNECT、WebSocket、TCP 透传四种代理协议
- **CMIN2 IP 库**：按 Cloudflare colo 代码分库存储，双层 IP 池（主池/备选池），支持健康检查淘汰和自动隔离
- **扫描器**：继承 cfdata 扫描管线，5 阶段处理（加载 CIDR → 抽样 → 探活 → 测速 → 入库），自动检测 IPv6 可用性
- **批量导入探测**：粘贴 IP 列表（FOFA/CF-Workers 导出结果），自动探活识别 colo、CMIN2 筛选、测速入库
- **WebUI 控制台**：暗色主题，纯静态（无外部 CDN 依赖），包含仪表盘、地区管理、CFNAT 配置、CFData、IP 库、日志、设置 7 个模块
- **热重载**：代理端口、地区配置、扫描参数、IP 库增删，全部运行时生效
- **持久化**：SQLite 单文件存储，Docker volume 挂载 `/data`
- **智能重试**：连接失败自动重试（可配置次数），最少连接数/加权随机负载均衡
- **连接亲和性**：同一客户端短时间内复用同一后端 IP，减少 TLS 握手开销
- **质量监控**：EWMA 实时监控延迟和丢包率，自动隔离劣化 IP

## 快速开始

```bash
git clone <repo>
cd cfnat-aio
docker compose up -d
```

访问 WebUI：`http://<服务器IP>:1234`

## 默认端口映射

`docker-compose.yml` 中预置了以下端口：

| 用途 | 宿主机端口 | 容器端口 |
|------|-----------|----------|
| WebUI | 1234 | 1234 |
| 香港代理 | 2001 | 1001 |
| 美国代理 | 2002 | 1002 |
| 日本代理 | 2003 | 1003 |
| 新加坡代理 | 2004 | 1004 |
| 越南代理 | 2005 | 1005 |

## 客户端配置示例

**qBittorrent**：设置 → 连接 → 代理类型 SOCKS5，主机 `<服务器IP>`，端口 `2003`（日本）

**v2rayN**：
- SOCKS5 模式：添加 SOCKS5 服务器，地址 `<服务器IP>`，端口 `2003`，无需认证
- WebSocket 模式：支持 WebSocket + TLS + cfnat 协议，代理端口 `2003`，系统自动识别握手并返回 101 响应

**Transmission**：编辑配置文件 `proxy` 段，类型 socks5，地址 `<服务器IP>`，端口 `2002`（美国）

支持 SOCKS5、HTTP CONNECT、WebSocket、TCP 透传四种代理协议。

## WebUI 模块

| 模块 | 功能 |
|------|------|
| 仪表盘 | 统计卡片、地区状态卡片（健康状态、IP 池数量）、实时日志流（SSE） |
| 地区管理 | 增删改代理地区，配置 colo 代码、端口、启用状态 |
| CFNAT 配置 | 代理转发参数配置（TLS、随机选取、IP 池大小等） |
| CFData | 扫描参数配置、手动触发扫描、扫描进度、扫描历史 |
| IP 库 | 查看/筛选/手动添加/删除 IP，按地区和来源筛选，导出功能 |
| 日志 | 实时日志流，按来源/级别筛选 |
| 设置 | WebUI 端口、日志级别、自动启动、API Token + 代理转发高级参数 |

## 目录结构

```
cfnat-aio/
├── cmd/server/          # main 入口
├── internal/
│   ├── config/          # 配置管理 + SQLite 存储
│   ├── iplibrary/       # CMIN2 IP 库（双层池 + 缓存 + DB）
│   ├── scanner/         # IP 扫描器（cfdata 继承）
│   ├── proxy/           # 代理转发（cfnat 继承，SOCKS5/HTTP/WebSocket/透传）
│   │   ├── manager.go   # 代理管理核心（重试、负载均衡、WebSocket 支持）
│   │   ├── metrics.go   # EWMA 质量监控
│   │   └── sticky.go    # 连接亲和性
│   ├── logging/         # 统一日志（内存 ring buffer + SSE）
│   └── webui/           # WebUI HTTP 处理器 + Vue 3 模板
├── data/                # SQLite 数据库和日志（.gitignore）
├── Dockerfile
├── Dockerfile.local     # 本地交叉编译构建
├── docker-compose.yml
├── 更新记录.md           # 程序更新记录
├── 产品版本迭代方案.md    # 产品版本迭代方案
├── 产品设计文档.md       # 功能模块、数据模型、技术架构
├── 使用说明文档.md       # 部署、配置、扫描、客户端连接、常见问题
└── .github/workflows/build.yml
```

## 数据流

```
Cloudflare CIDR 列表（官网 / 内置 fallback）
   ↓
抽样（每 /24 抽取 N 个）
   ↓
TCP 探活 + TLS 握手 + /cdn-cgi/trace 识别 colo
   ↓
测速（下载 speed.cloudflare.com）
   ↓
CMIN2 IP 库（双层池：主池活跃转发 + 备选池待命补充）
   ↓ 加权随机选取（考虑速度、延迟、连接数）
代理监听（每地区一个端口，自动协议检测：SOCKS5/HTTP/WebSocket/透传）
   ↓
客户端（qBittorrent / v2rayN / Transmission 等）
```

外部 IP 来源（FOFA / CF-Workers 导出）可通过"批量导入探测"直接注入上述数据流。

**支持协议**：

| 协议 | 特点 | 适用场景 |
|------|------|---------|
| SOCKS5 | 标准 SOCKS5 协议，无需认证 | qBittorrent、浏览器代理插件等 |
| HTTP CONNECT | 标准 HTTP 代理 | 浏览器、wget、curl 等 |
| WebSocket | 基于 HTTP 的 WebSocket 握手 | v2rayN 等 WebSocket 客户端 |
| TCP 透传 | 直接 TCP 转发 | 自定义协议、特殊客户端 |

## 文档

- [更新记录](更新记录.md) — 所有版本的功能更新、技术变更和 bug 修复
- [产品版本迭代方案](产品版本迭代方案.md) — V1.1~V1.7 功能迭代规划
- [产品设计文档](产品设计文档.md) — 功能模块、数据模型、技术架构
- [使用说明文档](使用说明文档.md) — 部署、配置、扫描、客户端连接、常见问题

## License

MIT