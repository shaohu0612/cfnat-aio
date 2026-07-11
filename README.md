# CFNAT-AIO

**All-In-One Cloudflare CMIN2 优选 IP 工具** — 整合 cfnat 代理 + cfdata 扫描 + FOFA 搜索 + WebUI 控制台，单容器单进程。

## 特性

- **多地区代理**：每个地区独立端口（如 HKG→:1001、LAX→:1002、JP→:1003），WebUI 动态增删，无需重启
- **CMIN2 IP 库**：按地区分库，只保留测速合格的 IP，自动淘汰失效
- **扫描器**：继承 cfdata 管线，改进点：
  - 每 /24 抽样数可配（1/3/5/全测）
  - 保留所有 /24 测试记录
  - 增量更新（优先复测上次好的 IP）
  - 扫描结果为空时 fallback 到全量随机
- **FOFA 集成**：手动触发，多 key 轮换（绕过配额），预设搜索模板
- **WebUI**：暗色主题，纯静态（无外部依赖），:1234 端口
- **热重载**：改代理、改地区、改扫描参数、增删 IP，全部不重启
- **持久化**：SQLite 单文件，Docker volume 挂载 `/vol1/1000/docker/cfnat-aio`

## 部署

```bash
git clone <repo>
cd cfnat-aio
docker compose up -d --build
```

访问：http://192.168.7.4:1234

## 客户端配置

```
qBittorrent:  SOCKS5 192.168.7.4:1001   # 走 HKG
Transmission: SOCKS5 192.168.7.4:1002   # 走 LAX
```

## 目录结构

```
cfnat-aio/
├── cmd/server/         # main 入口
├── internal/
│   ├── config/         # 配置 + SQLite 存储
│   ├── iplibrary/      # CMIN2 IP 库管理
│   ├── scanner/        # 扫描器（继承 cfdata）
│   ├── proxy/          # 代理转发（继承 cfnat）
│   ├── fofa/           # FOFA 客户端
│   └── webui/          # WebUI 处理器 + 模板
├── Dockerfile
├── docker-compose.yml
└── .github/workflows/build.yml
```

## 数据流

```
FOFA API（手动）
   ↓ 候选 IP
扫描器（自动 / 手动）
   ↓ TCP 探活 + 测速
CMIN2 IP 库（按地区分库）
   ↓ 随机选取
代理监听（每地区一个端口）
   ↓ SOCKS5
qBittorrent / Transmission / 其他客户端
```

## License

MIT
