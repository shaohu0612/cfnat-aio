# CFNAT-AIO

**All-In-One Cloudflare CMIN2 优选 IP 工具** — 整合 cfnat 代理转发、cfdata IP 扫描与 WebUI 控制台于单一容器进程。

## 特性

- **多地区代理**：每个地区独立 SOCKS5/HTTP 端口，WebUI 动态增删改，配置变更无需重启
- **CMIN2 IP 库**：按 Cloudflare colo 代码分库存储，只保留测速达标的 IP，支持健康检查淘汰
- **扫描器**：继承 cfdata 扫描管线，5 阶段处理（加载 CIDR → 抽样 → 探活 → 测速 → 入库）
- **批量导入探测**：粘贴 IP 列表（FOFA/CF-Workers 导出结果），自动探活识别 colo、CMIN2 筛选、测速入库
- **WebUI 控制台**：暗色主题，纯静态（无外部 CDN 依赖），包含仪表盘、地区管理、IP 库、扫描任务、系统设置 5 个模块
- **热重载**：代理端口、地区配置、扫描参数、IP 库增删，全部运行时生效
- **持久化**：SQLite 单文件存储，Docker volume 挂载 `/data`

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

**v2rayN**：添加 SOCKS5 服务器，地址 `<服务器IP>`，端口 `2003`，无需认证

**Transmission**：编辑配置文件 `proxy` 段，类型 socks5，地址 `<服务器IP>`，端口 `2002`（美国）

任何支持 SOCKS5 或 HTTP CONNECT 代理的客户端均可使用。

## WebUI 模块

| 模块 | 功能 |
|------|------|
| 仪表盘 | 地区状态卡片、可用 IP 统计、实时日志流（SSE） |
| 地区管理 | 增删改代理地区，配置 colo 代码、端口、启用状态 |
| IP 库 | 查看/筛选/手动添加/删除 IP，按地区名称和来源筛选 |
| 扫描任务 | 配置扫描参数、手动触发扫描、查看扫描历史 |
| 批量导入 | 粘贴 IP 列表自动探测 colo 和测速，可选自动入库 |
| 系统设置 | WebUI 端口、日志级别、自动启动、API Token |

## 目录结构

```
cfnat-aio/
├── cmd/server/         # main 入口
├── internal/
│   ├── config/         # 配置管理 + SQLite 存储
│   ├── iplibrary/      # CMIN2 IP 库（缓存 + DB）
│   ├── scanner/        # IP 扫描器（cfdata 继承）
│   ├── proxy/          # 代理转发（cfnat 继承，SOCKS5/HTTP/透传）
│   ├── logging/        # 统一日志（内存 ring buffer + SSE）
│   └── webui/          # WebUI HTTP 处理器 + Vue 3 模板
├── data/               # SQLite 数据库和日志（.gitignore）
├── Dockerfile
├── docker-compose.yml
├── 产品设计文档v1.0.md
├── 产品使用说明文档.md
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
CMIN2 IP 库（按 colo 分库，速度降序排列）
   ↓ 随机选取
代理监听（每地区一个端口，SOCKS5/HTTP/透传）
   ↓
客户端（qBittorrent / v2rayN / Transmission 等）
```

外部 IP 来源（FOFA / CF-Workers 导出）可通过"批量导入探测"直接注入上述数据流。

## 文档

- [产品设计文档 v1.0](产品设计文档v1.0.md) — 功能模块、数据模型、技术架构、已知限制
- [产品使用说明文档](产品使用说明文档.md) — 部署、配置、扫描、客户端连接、常见问题

## License

MIT
