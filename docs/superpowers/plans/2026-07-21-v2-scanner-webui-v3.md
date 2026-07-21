# V2.0 扫描引擎重构 + WebUI V3 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 解决"找不到 IP"的核心问题，并将 WebUI 重构为 V3 设计风格

**Architecture:** 后端采用 Go 标准库 + SQLite，前端采用 Tailwind CSS + Lucide Icons 单文件部署

**Tech Stack:** Go 1.22, SQLite, Tailwind CSS 4.x (CDN), Lucide Icons (CDN), 原生 JavaScript

---

## 文件结构

### 新建文件
- `internal/scanner/cidr_map.go` — /24 段到 colo 的映射表管理
- `internal/scanner/colostate.go` — Colo 扫描状态与预算管理
- `internal/config/datacenter.go` — 数据中心字典 API

### 修改文件
- `internal/scanner/scanner.go` — 扫描引擎核心逻辑
- `internal/config/config.go` — 扫描配置结构调整
- `internal/config/store_sqlite.go` — 新增数据表
- `internal/webui/webui.go` — 新增 API 端点
- `internal/webui/templates/index.html` — WebUI V3 重构

---

## Task 1: 扫描配置结构调整

**Files:**
- Modify: `internal/config/config.go:30-80`

### [ ] Step 1: 更新 ScannerConfig 结构体

找到 `ScannerConfig` 结构体定义（约第 30-50 行），修改为：

```go
// ScannerConfig 扫描器配置
type ScannerConfig struct {
	Interval         int     `json:"interval"`          // 扫描间隔（分钟），0=仅手动触发
	MinSpeedMBps    float64 `json:"minSpeedMBps"`      // 合格速度阈值（MB/s）
	IPType          int     `json:"ipType"`            // 4=IPv4, 6=IPv6
	Port            int     `json:"port"`              // 测速端口
	SamplesPer24    int     `json:"samplesPer24"`      // 每个 /24 段抽样数量
	MaxDelayMs      int     `json:"maxDelayMs"`        // 最大延迟阈值（ms）
	Threads         int     `json:"threads"`           // 探活并发数
	SpeedTestURL    string  `json:"speedTestUrl"`      // 测速网址，auto=自动选择
	OnlyCMIN2       bool    `json:"onlyCMIN2"`         // 是否仅限 CMIN2 colo
	SpeedTestMode   string  `json:"speedTestMode"`     // speed/latency/both
	ColoAware       bool    `json:"coloAware"`         // 是否启用分层精准扫描
	MapRebuildInterval int  `json:"mapRebuildInterval"` // 映射表重建间隔（小时）
	TargetIPsPerColo  int   `json:"targetIPsPerColo"`  // 每个 colo 目标 IP 数
	ExploreRatio    float64 `json:"exploreRatio"`      // 随机探索比例
}
```

### [ ] Step 2: 更新默认配置值

找到 `DefaultScannerConfig()` 函数（约第 80-100 行），修改为：

```go
func DefaultScannerConfig() ScannerConfig {
	return ScannerConfig{
		Interval:            60,
		MinSpeedMBps:       0.5,    // 从 3.0 改为 0.5
		IPType:             4,
		Port:               443,
		SamplesPer24:       100,
		MaxDelayMs:         500,
		Threads:            100,
		SpeedTestURL:       "auto",
		OnlyCMIN2:          false,  // 从 true 改为 false
		SpeedTestMode:      "speed",
		ColoAware:          true,
		MapRebuildInterval: 24,
		TargetIPsPerColo:   5,
		ExploreRatio:       0.1,
	}
}
```

### [ ] Step 3: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 2: 本地 IP 文件优先加载

**Files:**
- Modify: `internal/scanner/scanner.go:50-120`

### [ ] Step 1: 添加 loadLocalCIDRs 函数

在 `scanner.go` 文件顶部的 import 区域后，添加新函数：

```go
// loadLocalCIDRs 从本地文件加载 CIDR 列表
func (s *Scanner) loadLocalCIDRs(filename string) ([]string, error) {
	// 尝试多个路径
	paths := []string{
		filename,
		filepath.Join("/data", filename),
		filepath.Join(".", filename),
	}
	
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		
		lines := strings.Split(string(data), "\n")
		var cidrs []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// 验证 CIDR 格式
			if _, _, err := net.ParseCIDR(line); err == nil {
				cidrs = append(cidrs, line)
			}
		}
		
		if len(cidrs) > 0 {
			logging.InfoTo("scanner", "使用本地 IP 库: %s (%d 段)", p, len(cidrs))
			return cidrs, nil
		}
	}
	
	return nil, os.ErrNotExist
}
```

### [ ] Step 2: 修改 loadCIDRs 函数优先使用本地文件

找到 `loadCIDRs` 函数（约第 70-120 行），在开头添加本地文件优先逻辑：

```go
func (s *Scanner) loadCIDRs(ipType int) ([]string, error) {
	// 1. 优先加载本地文件
	localFile := "ip.txt"
	if ipType == 6 {
		localFile = "ip6.txt"
	}
	
	if cidrs, err := s.loadLocalCIDRs(localFile); err == nil && len(cidrs) > 0 {
		s.progress.CIDRSource = "local"
		s.progress.CIDRFile = localFile
		s.progress.CIDRSegments = len(cidrs)
		return cidrs, nil
	}
	
	logging.InfoTo("scanner", "本地 IP 库不存在，使用远程 URL")
	
	// 2. 回退到远程 URL（保留现有逻辑）
	// ... 现有的远程加载代码 ...
}
```

### [ ] Step 3: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 3: /24 抽样逻辑修复

**Files:**
- Modify: `internal/scanner/scanner.go:200-280`

### [ ] Step 1: 添加 splitTo24 函数

```go
// splitTo24 将 CIDR 拆分为 /24 段
func splitTo24(cidr string) []string {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	
	ones, _ := ipnet.Mask.Size()
	if ones >= 24 {
		// 已经是 /24 或更小，不需要拆分
		return []string{cidr}
	}
	
	// 计算需要拆分的数量
	bitsToSplit := 24 - ones
	count := 1 << bitsToSplit
	
	var result []string
	baseIP := ip.To4()
	if baseIP == nil {
		// IPv6 不拆分
		return []string{cidr}
	}
	
	// 计算 /24 段的起始地址
	baseInt := uint32(baseIP[0])<<24 | uint32(baseIP[1])<<16 | uint32(baseIP[2])<<8 | uint32(baseIP[3])
	// 对齐到 /24 边界
	baseInt = baseInt & 0xFFFFFF00
	
	for i := 0; i < count; i++ {
		offset := uint32(i * 256)
		newIP := make(net.IP, 4)
		newInt := baseInt + offset
		newIP[0] = byte(newInt >> 24)
		newIP[1] = byte(newInt >> 16)
		newIP[2] = byte(newInt >> 8)
		newIP[3] = byte(newInt)
		result = append(result, fmt.Sprintf("%s/24", newIP.String()))
	}
	
	return result
}
```

### [ ] Step 2: 重写 sampleCIDRs 函数

找到 `sampleCIDRs` 函数（约第 200-250 行），完全重写为：

```go
// sampleCIDRs 对 CIDR 列表进行抽样，真正按 /24 粒度
func (s *Scanner) sampleCIDRs(cidrs []string, samplesPer24, ipType int) []string {
	// 1. 将所有 CIDR 统一拆分为 /24 段
	var cidr24s []string
	for _, c := range cidrs {
		splits := splitTo24(c)
		cidr24s = append(cidr24s, splits...)
	}
	
	logging.InfoTo("scanner", "拆分为 %d 个 /24 段", len(cidr24s))
	s.progress.CIDR24Segments = len(cidr24s)
	
	// 2. 对每个 /24 抽样
	var candidates []string
	for _, c24 := range cidr24s {
		for i := 0; i < samplesPer24; i++ {
			ip := s.randomIPInCIDR(c24)
			if ip != "" {
				candidates = append(candidates, ip)
			}
		}
	}
	
	logging.InfoTo("scanner", "生成 %d 个候选 IP", len(candidates))
	return candidates
}

// randomIPInCIDR 从 CIDR 中随机抽取一个 IP
func (s *Scanner) randomIPInCIDR(cidr string) string {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits == 0 {
		return ip.String()
	}
	
	// 生成随机偏移
	offset := rand.Intn(1<<hostBits - 2) + 1 // 避免网络地址和广播地址
	
	ipBytes := ip.To4()
	if ipBytes == nil {
		// IPv6
		ipBytes = ip.To16()
	}
	
	// 计算新 IP
	ipInt := big.NewInt(0).SetBytes(ipBytes)
	offsetInt := big.NewInt(int64(offset))
	ipInt.Add(ipInt, offsetInt)
	
	newIP := net.IP(ipInt.Bytes())
	return newIP.String()
}
```

### [ ] Step 3: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 4: 测速模式切换

**Files:**
- Modify: `internal/scanner/scanner.go:400-500`

### [ ] Step 1: 添加测速模式分发逻辑

找到测速相关代码（约第 400-500 行），添加模式判断：

```go
// speedTest 根据模式执行测速
func (s *Scanner) speedTest(ctx context.Context, probes []ProbeResult, sc config.ScannerConfig) []speedResult {
	var results []speedResult
	
	switch sc.SpeedTestMode {
	case "latency":
		// 只测延迟，不测下载速度
		for _, p := range probes {
			if p.OK {
				results = append(results, speedResult{
					IP:         p.IP,
					Colo:       p.Colo,
					LatencyMs:  p.LatencyMs,
					SpeedMBps:  0, // 不测速
					OK:         true,
				})
			}
		}
		
	case "both":
		// 先测延迟，延迟达标再测速度
		for _, p := range probes {
			if !p.OK || p.LatencyMs > float64(sc.MaxDelayMs) {
				continue
			}
			// 执行实际测速
			speed, ok := s.measureSpeed(p.IP, sc.SpeedTestURL, sc.Port)
			results = append(results, speedResult{
				IP:        p.IP,
				Colo:      p.Colo,
				LatencyMs: p.LatencyMs,
				SpeedMBps: speed,
				OK:        ok,
			})
		}
		
	default: // "speed"
		// 现有逻辑：全部测速
		for _, p := range probes {
			if !p.OK {
				continue
			}
			speed, ok := s.measureSpeed(p.IP, sc.SpeedTestURL, sc.Port)
			results = append(results, speedResult{
				IP:        p.IP,
				Colo:      p.Colo,
				LatencyMs: p.LatencyMs,
				SpeedMBps: speed,
				OK:        ok,
			})
		}
	}
	
	return results
}
```

### [ ] Step 2: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 5: Region 映射修正

**Files:**
- Modify: `internal/scanner/scanner.go:500-600`

### [ ] Step 1: 添加 findRegionByColo 函数

```go
// findRegionByColo 根据 colo 查找所属地区名称
func (s *Scanner) findRegionByColo(colo string, regions []config.ProxyRegion) string {
	for _, r := range regions {
		codes := parseCodes(r.Code)
		for _, code := range codes {
			if code == colo {
				return r.Name
			}
		}
	}
	// 未找到，返回 colo 作为 region
	return colo
}

// parseCodes 解析逗号分隔的 colo 代码
func parseCodes(codeStr string) []string {
	parts := strings.Split(codeStr, ",")
	var codes []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			codes = append(codes, p)
		}
	}
	return codes
}
```

### [ ] Step 2: 修改 saveByRegion 函数

找到 `saveByRegion` 函数（约第 550-600 行），修改入库逻辑：

```go
func (s *Scanner) saveByRegion(passed []speedResult, sc config.ScannerConfig) map[string]int {
	out := make(map[string]int)
	regions := s.cfgMgr.Regions()
	
	for _, r := range passed {
		if !r.OK || r.SpeedMBps < sc.MinSpeedMBps {
			continue
		}
		
		// 查找该 colo 属于哪个地区配置
		regionName := s.findRegionByColo(r.Colo, regions)
		if regionName == r.Colo {
			// 未找到匹配的地区配置，记录警告
			logging.WarnTo("scanner", "Colo %s 不在任何地区配置中，作为独立 region 入库", r.Colo)
		}
		
		err := s.lib.AddIP(r.IP, regionName, "auto", r.Colo, r.SpeedMBps, r.LatencyMs, "scanner")
		if err == nil {
			out[regionName]++
		}
	}
	
	return out
}
```

### [ ] Step 3: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 6: 数据库表结构扩展

**Files:**
- Modify: `internal/config/store_sqlite.go:50-150`

### [ ] Step 1: 添加新表结构

在 `initDB` 函数的表创建语句中添加：

```go
// /24 段到 colo 的映射表（Colo-Aware Scanning）
`CREATE TABLE IF NOT EXISTS cidr_colo_map (
    cidr24     TEXT PRIMARY KEY,
    colo       TEXT NOT NULL,
    probed_at  TEXT,
    ok_count   INTEGER DEFAULT 0,
    fail_count INTEGER DEFAULT 0,
    confidence REAL DEFAULT 0.0
)`,
`CREATE INDEX IF NOT EXISTS idx_cidr_colo ON cidr_colo_map(colo)`,

// Colo 扫描状态表
`CREATE TABLE IF NOT EXISTS colo_scan_state (
    colo        TEXT PRIMARY KEY,
    region      TEXT NOT NULL,
    budget      INTEGER DEFAULT 10,
    current_ips INTEGER DEFAULT 0,
    target_ips  INTEGER DEFAULT 5,
    last_scan   TEXT
)`,
`CREATE INDEX IF NOT EXISTS idx_colo_region ON colo_scan_state(region)`,
```

### [ ] Step 2: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 7: 扫描进度结构扩展

**Files:**
- Modify: `internal/scanner/scanner.go:30-50`

### [ ] Step 1: 扩展 Progress 结构体

找到 `Progress` 结构体（约第 30-50 行），添加新字段：

```go
// Progress 扫描进度
type Progress struct {
	mu             sync.RWMutex
	Stage          string  `json:"stage"`
	Current        int     `json:"current"`
	Total          int     `json:"total"`
	CurrentIP      string  `json:"currentIp"`
	Percent        float64 `json:"percent"`
	Elapsed        int64   `json:"elapsed"`  // 秒
	CIDRSource     string  `json:"cidrSource"`     // local/remote
	CIDRFile       string  `json:"cidrFile"`       // 文件名
	CIDRSegments   int     `json:"cidrSegments"`   // 原始 CIDR 段数
	CIDR24Segments int     `json:"cidr24Segments"` // /24 段数
	ProbePass      int     `json:"probePass"`
	ProbeFail      int     `json:"probeFail"`
	SpeedPass      int     `json:"speedPass"`
	SpeedFail      int     `json:"speedFail"`
	Saved          int     `json:"saved"`
	Error          string  `json:"error"`
}
```

### [ ] Step 2: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 8: WebUI API 扩展

**Files:**
- Modify: `internal/webui/webui.go:100-300`

### [ ] Step 1: 添加扫描进度 API

在路由注册区域添加：

```go
// 扫描进度 API（扩展）
r.HandleFunc("/api/scanner/progress", s.handleScannerProgress)

func (s *Server) handleScannerProgress(w http.ResponseWriter, r *http.Request) {
	progress := s.scanner.GetProgress()
	
	// 返回扩展字段
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"stage":          progress.Stage,
			"current":        progress.Current,
			"total":          progress.Total,
			"currentIp":      progress.CurrentIP,
			"percent":        progress.Percent,
			"elapsed":        progress.Elapsed,
			"cidrSource":     progress.CIDRSource,
			"cidrFile":       progress.CIDRFile,
			"cidrSegments":   progress.CIDRSegments,
			"cidr24Segments": progress.CIDR24Segments,
			"probePass":      progress.ProbePass,
			"probeFail":      progress.ProbeFail,
			"speedPass":      progress.SpeedPass,
			"speedFail":      progress.SpeedFail,
			"saved":          progress.Saved,
			"error":          progress.Error,
		},
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
```

### [ ] Step 2: 添加扫描配置更新 API

```go
r.HandleFunc("/api/scanner/config", s.handleScannerConfig)

func (s *Server) handleScannerConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// 返回当前配置
		cfg := s.cfgMgr.Scanner()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":    0,
			"message": "success",
			"data":    cfg,
		})
		return
	}
	
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		// 更新配置
		var cfg config.ScannerConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		
		if err := s.cfgMgr.SaveScannerConfig(cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code":    0,
			"message": "配置已保存",
		})
	}
}
```

### [ ] Step 3: 验证编译

```bash
cd /e/cfnat-aio && go build ./cmd/server
```

---

## Task 9: WebUI V3 重构 - 基础框架

**Files:**
- Modify: `internal/webui/templates/index.html:1-500`

### [ ] Step 1: 重写 HTML 头部和样式

将现有 Vue/Naive UI 代码完全替换为 Tailwind CSS + Lucide Icons：

```html
<!DOCTYPE html>
<html lang="zh-CN" class="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CFNAT-AIO V3</title>
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4.3.1/dist/index.global.js"></script>
<script src="https://unpkg.com/lucide@1.8.0/dist/umd/lucide.min.js"></script>
<style>
/* CFNAT-AIO V3 Design System */
:root {
  --cfnat-primary: #58a6ff;
  --cfnat-primary-hover: #79b8ff;
  --cfnat-primary-pressed: #1f6feb;
  --cfnat-primary-foreground: #ffffff;
  --cfnat-accent: #a78bfa;
  --cfnat-accent-hover: #c4b5fd;
  --cfnat-accent-pressed: #7c3aed;
  --cfnat-background: #0a0e14;
  --cfnat-foreground: #e6edf3;
  --cfnat-card: #12161e;
  --cfnat-card-foreground: #c9d1d9;
  --cfnat-popover: #1a1f2e;
  --cfnat-popover-foreground: #c9d1d9;
  --cfnat-muted: #161b22;
  --cfnat-muted-foreground: #6e7681;
  --cfnat-border: #1e2430;
  --cfnat-input: #0d1117;
  --cfnat-ring: #58a6ff;
  --cfnat-radius-sm: 4px;
  --cfnat-radius-md: 8px;
  --cfnat-radius-lg: 12px;
  --cfnat-radius-full: 9999px;
  --state-success: #3fb950;
  --state-warning: #d29922;
  --state-error: #f85149;
  --state-info: #58a6ff;
  --font-display: "SF Mono", "Cascadia Code", "Fira Code", "Consolas", monospace;
  --font-body: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Helvetica Neue", sans-serif;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: var(--font-body); background: var(--cfnat-background); min-height: 100vh; color: var(--cfnat-foreground); }
</style>
<style type="text/tailwindcss">
@theme inline {
  --color-background: var(--cfnat-background);
  --color-foreground: var(--cfnat-foreground);
  --color-card: var(--cfnat-card);
  --color-card-foreground: var(--cfnat-card-foreground);
  --color-primary: var(--cfnat-primary);
  --color-accent: var(--cfnat-accent);
  --color-muted: var(--cfnat-muted);
  --color-muted-foreground: var(--cfnat-muted-foreground);
  --color-border: var(--cfnat-border);
  --radius-sm: var(--cfnat-radius-sm);
  --radius-md: var(--cfnat-radius-md);
  --radius-lg: var(--cfnat-radius-lg);
}
</style>
</head>
<body class="min-h-screen font-sans antialiased">
```

### [ ] Step 2: 添加导航栏组件

```html
<!-- 顶部导航栏 -->
<nav style="background:var(--cfnat-card);border-bottom:1px solid var(--cfnat-border);" class="px-4 py-3 flex items-center justify-between">
  <div class="flex items-center gap-3">
    <span class="text-lg font-bold tracking-tight" style="color:var(--cfnat-primary);font-family:var(--font-display);">CFNAT-AIO</span>
    <span class="text-xs px-2 py-0.5 rounded-md font-medium" style="background:var(--cfnat-accent);color:#fff;">V3</span>
    <span style="width:1px;height:20px;background:var(--cfnat-border);"></span>
    <span class="text-sm" style="color:var(--cfnat-muted-foreground);">智能代理控制台</span>
  </div>
  <div class="hidden md:flex items-center gap-1">
    <a href="#dashboard" class="nav-link px-3 py-1.5 text-sm font-medium rounded-md" style="background:var(--cfnat-primary);color:var(--cfnat-primary-foreground);" data-page="dashboard">仪表盘</a>
    <a href="#regions" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="regions">地区管理</a>
    <a href="#cfnat" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="cfnat">cfnat 配置</a>
    <a href="#cfdata" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="cfdata">cfdata</a>
    <a href="#ips" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="ips">IP 库</a>
    <a href="#logs" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="logs">日志</a>
    <a href="#settings" class="nav-link px-3 py-1.5 text-sm rounded-md" style="color:var(--cfnat-muted-foreground);" data-page="settings">通用设置</a>
  </div>
</nav>
```

### [ ] Step 3: 验证页面渲染

```bash
cd /e/cfnat-aio && go build ./cmd/server && docker compose up -d
```

访问 `http://localhost:1234` 确认基础框架正常显示。

---

## Task 10: WebUI V3 仪表盘页面

**Files:**
- Modify: `internal/webui/templates/index.html:500-1200`

### [ ] Step 1: 添加仪表盘页面模板

基于 V3 设计文档 `dashboard.html`，添加完整的仪表盘页面：

```html
<!-- 仪表盘页面 -->
<div id="page-dashboard" class="page-content p-4 lg:p-6 max-w-screen-2xl mx-auto space-y-4 lg:space-y-6">
  
  <!-- 统计卡片行 -->
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 lg:gap-4" id="stat-cards">
    <!-- 运行时间 -->
    <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4">
      <div class="flex items-center gap-2 mb-2">
        <i data-lucide="clock" class="w-4 h-4" style="color:var(--cfnat-muted-foreground);"></i>
        <span class="text-xs" style="color:var(--cfnat-muted-foreground);">运行时间</span>
      </div>
      <div class="text-2xl font-bold" id="stat-uptime" style="color:var(--cfnat-foreground);">--</div>
    </div>
    <!-- 总 IP 数 -->
    <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4">
      <div class="flex items-center gap-2 mb-2">
        <i data-lucide="server" class="w-4 h-4" style="color:var(--cfnat-muted-foreground);"></i>
        <span class="text-xs" style="color:var(--cfnat-muted-foreground);">总 IP 数</span>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-2xl font-bold" id="stat-total-ips" style="color:var(--cfnat-foreground);">0</span>
        <span class="inline-block w-2 h-2 rounded-full" style="background:var(--state-success);"></span>
      </div>
    </div>
    <!-- 代理监听 -->
    <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4">
      <div class="flex items-center gap-2 mb-2">
        <i data-lucide="radio" class="w-4 h-4" style="color:var(--cfnat-muted-foreground);"></i>
        <span class="text-xs" style="color:var(--cfnat-muted-foreground);">代理监听 / 地区</span>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-2xl font-bold" id="stat-listeners" style="color:var(--cfnat-foreground);">--</span>
        <span class="inline-block w-2 h-2 rounded-full animate-pulse" style="background:var(--state-success);"></span>
      </div>
    </div>
    <!-- cfdata 状态 -->
    <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4">
      <div class="flex items-center gap-2 mb-2">
        <i data-lucide="scan" class="w-4 h-4" style="color:var(--cfnat-accent);"></i>
        <span class="text-xs" style="color:var(--cfnat-muted-foreground);">cfdata 状态</span>
      </div>
      <div class="flex items-center gap-2 mb-1">
        <span class="text-2xl font-bold" id="stat-scanner-status" style="color:var(--cfnat-foreground);">--</span>
        <span class="inline-block w-2 h-2 rounded-full animate-pulse" id="scanner-indicator" style="background:var(--cfnat-muted-foreground);"></span>
      </div>
      <span class="inline-block px-2 py-0.5 text-xs whitespace-nowrap" id="scanner-mode-badge" style="background:color-mix(in srgb,var(--cfnat-muted) 15%,transparent);color:var(--cfnat-muted-foreground);border-radius:var(--cfnat-radius-sm);">--</span>
    </div>
  </div>
  
  <!-- 地区代理状态卡片 -->
  <div class="grid grid-cols-1 lg:grid-cols-2 gap-3 lg:gap-4" id="region-cards">
    <!-- 由 JavaScript 动态生成 -->
  </div>
  
  <!-- 自适应阈值面板 -->
  <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4 lg:p-5">
    <div class="flex items-center gap-2 mb-4">
      <i data-lucide="sliders-horizontal" class="w-4 h-4" style="color:var(--cfnat-accent);"></i>
      <span class="text-sm font-semibold" style="color:var(--cfnat-foreground);">自适应阈值状态</span>
    </div>
    <div class="space-y-3">
      <div class="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
        <span style="color:var(--cfnat-muted-foreground);">当前环境评估：</span>
        <span class="px-2 py-0.5 whitespace-nowrap" id="env-type-badge" style="background:color-mix(in srgb,var(--cfnat-primary) 15%,transparent);color:var(--cfnat-primary);border-radius:var(--cfnat-radius-sm);">--</span>
        <span style="color:var(--cfnat-muted-foreground);">延迟基线</span>
        <span class="font-mono font-medium" id="baseline-latency" style="color:var(--cfnat-foreground);">--ms</span>
      </div>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <div class="flex items-center justify-between p-3" style="background:var(--cfnat-muted);border-radius:var(--cfnat-radius-md);">
          <span class="text-xs truncate" style="color:var(--cfnat-muted-foreground);">自适应延迟阈值</span>
          <span class="text-sm font-bold font-mono whitespace-nowrap" id="adaptive-delay" style="color:var(--cfnat-foreground);">--ms</span>
        </div>
        <div class="flex items-center justify-between p-3" style="background:var(--cfnat-muted);border-radius:var(--cfnat-radius-md);">
          <span class="text-xs truncate" style="color:var(--cfnat-muted-foreground);">自适应速度阈值</span>
          <span class="text-sm font-bold font-mono whitespace-nowrap" id="adaptive-speed" style="color:var(--cfnat-foreground);">-- MB/s</span>
        </div>
        <div class="flex items-center justify-between p-3" style="background:var(--cfnat-muted);border-radius:var(--cfnat-radius-md);">
          <span class="text-xs truncate" style="color:var(--cfnat-muted-foreground);">新 IP 优先调度</span>
          <div class="flex items-center gap-1.5">
            <span class="inline-block w-2 h-2 rounded-full" id="priority-indicator" style="background:var(--state-success);"></span>
            <span class="text-sm font-bold whitespace-nowrap" id="priority-status" style="color:var(--state-success);">--</span>
          </div>
        </div>
      </div>
    </div>
  </div>
  
  <!-- 扫描诊断摘要 -->
  <div style="background:var(--cfnat-card);border:1px solid var(--cfnat-border);border-radius:var(--cfnat-radius-lg);" class="p-4 lg:p-5">
    <div class="flex items-center gap-2 mb-4">
      <i data-lucide="activity" class="w-4 h-4" style="color:var(--cfnat-accent);"></i>
      <span class="text-sm font-semibold" style="color:var(--cfnat-foreground);">最近扫描诊断</span>
    </div>
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-x-6 gap-y-3 text-sm" id="scan-diagnosis">
      <!-- 由 JavaScript 动态生成 -->
    </div>
  </div>
  
</div>
```

### [ ] Step 2: 添加页面切换 JavaScript

```html
<script>
// 页面状态
let currentPage = 'dashboard';
let refreshInterval = null;

// 页面切换
function switchPage(page) {
  // 隐藏所有页面
  document.querySelectorAll('.page-content').forEach(el => el.style.display = 'none');
  // 显示目标页面
  const targetPage = document.getElementById('page-' + page);
  if (targetPage) {
    targetPage.style.display = 'block';
  }
  // 更新导航栏状态
  document.querySelectorAll('.nav-link').forEach(el => {
    if (el.dataset.page === page) {
      el.style.background = 'var(--cfnat-primary)';
      el.style.color = 'var(--cfnat-primary-foreground)';
    } else {
      el.style.background = 'transparent';
      el.style.color = 'var(--cfnat-muted-foreground)';
    }
  });
  currentPage = page;
  // 触发页面数据加载
  loadPageData(page);
}

// 初始化
document.addEventListener('DOMContentLoaded', () => {
  lucide.createIcons();
  // 绑定导航事件
  document.querySelectorAll('.nav-link').forEach(el => {
    el.addEventListener('click', (e) => {
      e.preventDefault();
      switchPage(el.dataset.page);
    });
  });
  // 加载初始页面
  switchPage('dashboard');
  // 启动自动刷新
  startAutoRefresh();
});
</script>
```

---

## Task 11: Docker 构建与部署测试

**Files:**
- 无文件修改，执行命令

### [ ] Step 1: 停止并删除旧容器

```bash
docker stop cfnat-aio && docker rm cfnat-aio
```

### [ ] Step 2: 重新构建镜像

```bash
cd /e/cfnat-aio && docker build -t cfnat-aio:latest -f Dockerfile.local .
```

### [ ] Step 3: 启动容器

```bash
cd /e/cfnat-aio && docker compose up -d
```

### [ ] Step 4: 验证服务状态

```bash
docker logs cfnat-aio
curl http://localhost:1234/api/regions
curl http://localhost:1234/api/scanner/config
```

---

## Task 12: 功能测试与提交

**Files:**
- 无文件修改，执行测试

### [ ] Step 1: 执行测试用例 V20-001 到 V20-027

参考 `测试用例v2.md`，逐项验证：

- V20-001: 本地 ip.txt 优先加载
- V20-007: 大段 CIDR 正确拆分
- V20-013: 默认合格速度 0.5 MB/s
- V20-014: OnlyCMIN2 默认关闭
- V20-023: LAX IP 入库到地区名称

### [ ] Step 2: 记录测试结果

### [ ] Step 3: 提交代码

```bash
git add -A && git commit -m "feat: V2.0 扫描引擎重构 + WebUI V3 基础框架

- 本地 IP 文件优先加载（ip.txt/ip6.txt）
- /24 抽样逻辑修复
- 默认阈值调整：MinSpeedMBps=0.5, OnlyCMIN2=false
- 测速模式切换（speed/latency/both）
- Region 映射修正
- 新增 cidr_colo_map、colo_scan_state 数据表
- WebUI V3 基础框架（Tailwind CSS + Lucide Icons）
- 仪表盘页面重构"
git push origin main
```

---

## 执行选项

**Plan complete. Two execution options:**

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session, batch execution with checkpoints

**Which approach would you like?**