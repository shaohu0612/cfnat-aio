// Package config 统一管理 CFNAT-AIO 的运行时配置
//
// 配置以内存对象 + SQLite 持久化双写：
//   - 启动时从数据库加载，写入内存
//   - WebUI 修改时，先写 DB，再同步到内存（热更新）
//   - 进程退出时无需主动保存（任何修改都已落库）
package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"cfnat-aio/internal/logging"
)

// ProxyRegion 描述一个代理地区（监听端口 + CMIN2 IP 库）
type ProxyRegion struct {
	Name      string `json:"name"`       // HKG / LAX / JP
	Code      string `json:"code"`       // 数据中心代码（用于从扫描结果匹配）
	Port      int    `json:"port"`       // 监听端口
	Enabled   bool   `json:"enabled"`    // 是否启用
	Fallback  bool   `json:"fallback"`   // 库中 IP 全不可用时是否自动 fallback 到全量 CF
	IPCount   int    `json:"ip_count"`   // 当前可用 IP 数（运行时统计）
	LastCheck string `json:"last_check"` // 上次健康检查时间
}

// ScannerConfig 扫描器配置
type ScannerConfig struct {
	Enabled       bool    `json:"enabled"`         // 是否开启后台自动扫描
	Interval      int     `json:"interval"`        // 扫描间隔（分钟）
	MinSpeedMBps  float64 `json:"min_speed_mbps"`  // 测速合格阈值（MB/s）
	IPType        int     `json:"ip_type"`         // 4 或 6
	Port          int     `json:"port"`            // 测试端口
	SamplesPer24  int     `json:"samples_per_24"`  // 每 /24 抽样数（1/3/5/全测=255）
	MaxDelayMs    int     `json:"max_delay_ms"`    // 最大延迟阈值
	Threads       int     `json:"threads"`         // 并发数
	ScanMode      string  `json:"scan_mode"`       // tcping / httping
	SpeedTestURL  string  `json:"speed_test_url"`  // 测速下载 URL（auto=自动选）
	OnlyCMIN2          bool    `json:"only_cmin2"`           // 是否只保留 CMIN2 节点
	CMIN2Colos         string  `json:"cmin2_colos"`          // CMIN2 节点列表（逗号分隔）
	SpeedTestMode      string  `json:"speed_test_mode"`      // 测速模式：speed/latency/both
	ColoAware          bool    `json:"colo_aware"`           // 是否启用分层精准扫描
	MapRebuildInterval int     `json:"map_rebuild_interval"` // 映射表重建间隔（小时）
	TargetIPsPerColo   int     `json:"target_ips_per_colo"`  // 每个 colo 目标 IP 数
	ExploreRatio       float64 `json:"explore_ratio"`        // 随机探索比例
	NextRunTime        string  `json:"next_run_time"`        // 下次运行时间（运行时计算）
	LastRunTime        string  `json:"last_run_time"`        // 上次完成时间
	LastRunStatus      string  `json:"last_run_status"`      // 上次状态
	LastRunStats       string  `json:"last_run_stats"`       // 上次扫描统计 JSON
}

// CfnatConfig 代理转发配置（对应原 cfnat-docker-compose 环境变量）
type CfnatConfig struct {
	TLSMode    bool   `json:"tls_mode"`    // TLS 连接模式 (tls)
	RandomPick bool   `json:"random_pick"` // 随机选取 IP (random)
	IPPoolSize int    `json:"ip_pool_size"` // IP 池大小 (ipnum)
	ForwardNum int    `json:"forward_num"` // 转发 IP 轮换数 (num)
	SpeedTime  int    `json:"speed_time"`  // 测速时长秒 (speedtime)
	ExpectCode int    `json:"expect_code"` // 期望 HTTP 状态码 (code)
}

// GeneralConfig 通用配置
type GeneralConfig struct {
	WebUIPort    int    `json:"webui_port"`     // WebUI 监听端口
	APIToken     string `json:"api_token"`      // API 鉴权 token（可选）
	DataDir      string `json:"data_dir"`       // 数据目录（存放 cfnat-aio.db）
	LogLevel     string `json:"log_level"`      // debug/info/warn/error
	AutoStart    bool   `json:"auto_start"`     // 容器启动时是否自动开启扫描和代理
}

// ProxyForwardConfig 代理转发配置（V1.1+）
type ProxyForwardConfig struct {
	MaxRetries          int     `json:"max_retries"`           // 最大重试次数（0-5，默认3）
	LoadBalanceMode     string  `json:"load_balance_mode"`     // 负载均衡策略：random/least-conn/weighted-random
	EWMASampleWindow    int     `json:"ewma_sample_window"`    // EWMA样本窗口（20-200，默认50）
	HealthCheckInterval int     `json:"health_check_interval"` // 健康检查间隔（30-600s，默认120）
	MaxDelayMs          int     `json:"max_delay_ms"`          // 最大延迟阈值（100-2000ms，默认500）
	MaxLossRate         float64 `json:"max_loss_rate"`         // 最大丢包率阈值（1-50%，默认10%）
	IsolationDuration   int     `json:"isolation_duration"`    // 隔离时长（60-900s，默认300）
	WarmupDuration      int     `json:"warmup_duration"`       // 预热保护期（0-300s，默认60）
	StickyEnabled       bool    `json:"sticky_enabled"`        // 连接亲和性开关
	StickyTTL           int     `json:"sticky_ttl"`            // 亲和性TTL（5-60s，默认15）
	ActivePoolSize      int     `json:"active_pool_size"`      // 主池容量（5-100，默认20）
	StandbyPoolRatio    float64 `json:"standby_pool_ratio"`    // 备选池比例（0.2-1.0，默认0.5）
}

// Manager 全局配置管理器（线程安全）
type Manager struct {
	mu      sync.RWMutex
	general GeneralConfig
	scanner ScannerConfig
	cfnat   CfnatConfig
	proxyForward ProxyForwardConfig
	regions []ProxyRegion
	db      ConfigStore
}

// ConfigStore 配置持久化接口（解耦 DB 依赖）
type ConfigStore interface {
	LoadGeneral() (GeneralConfig, error)
	SaveGeneral(g GeneralConfig) error
	LoadScanner() (ScannerConfig, error)
	SaveScanner(s ScannerConfig) error
	LoadCfnat() (CfnatConfig, error)
	SaveCfnat(c CfnatConfig) error
	LoadProxyForward() (ProxyForwardConfig, error)
	SaveProxyForward(pf ProxyForwardConfig) error
	LoadRegions() ([]ProxyRegion, error)
	SaveRegions(regions []ProxyRegion) error
}

// New 创建配置管理器，从 DB 加载初始配置
func New(store ConfigStore) (*Manager, error) {
	m := &Manager{db: store}
	if err := m.loadAll(); err != nil {
		return nil, err
	}
	// 旧数据 region 迁移：将 colo 代码映射为地区名称
	if sqliteStore, ok := store.(*SQLiteStore); ok {
		_ = migrateOldRegion(sqliteStore.DB(), m.regions)
	}
	return m, nil
}

// migrateOldRegion 将 iplib_ip 表中旧 colo 代码 region 迁移为地区名称
func migrateOldRegion(db *sql.DB, regions []ProxyRegion) error {
	rows, err := db.Query(`SELECT DISTINCT region FROM iplib_ip`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// 构建 colo -> regionName 映射
	coloToRegion := make(map[string]string)
	for _, reg := range regions {
		codes := strings.Split(reg.Code, ",")
		for _, c := range codes {
			c = strings.TrimSpace(c)
			if c != "" {
				coloToRegion[c] = reg.Name
			}
		}
	}

	var toMigrate []struct{ old, new string }
	for rows.Next() {
		var oldRegion string
		if err := rows.Scan(&oldRegion); err != nil {
			continue
		}
		newRegion, ok := coloToRegion[oldRegion]
		if !ok || newRegion == oldRegion {
			continue
		}
		toMigrate = append(toMigrate, struct{ old, new string }{oldRegion, newRegion})
	}

	var migrated int
	for _, item := range toMigrate {
		res, err := db.Exec(`UPDATE iplib_ip SET region=? WHERE region=?`, item.new, item.old)
		if err != nil {
			logging.WarnTo("config", "旧数据 region 迁移失败 %s->%s: %v", item.old, item.new, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			migrated++
		}
	}
	if migrated > 0 {
		logging.InfoTo("config", "✅ 旧数据 region 迁移完成: %d 个 colo 代码已映射为地区名称", migrated)
	}
	return nil
}

func (m *Manager) loadAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if g, err := m.db.LoadGeneral(); err == nil {
		m.general = g
	} else {
		// 首次启动，使用默认值
		m.general = GeneralConfig{
			WebUIPort: 1234,
			DataDir:   "/data",
			LogLevel:  "info",
			AutoStart: true,
		}
		_ = m.db.SaveGeneral(m.general)
	}

	if s, err := m.db.LoadScanner(); err == nil {
		m.scanner = s
		// V2.0 配置迁移：speed_test_mode 为空说明是旧配置，用新默认值填充
		if m.scanner.SpeedTestMode == "" {
			m.scanner.SpeedTestMode = "speed"
			m.scanner.ColoAware = true
			m.scanner.MapRebuildInterval = 24
			m.scanner.TargetIPsPerColo = 5
			m.scanner.ExploreRatio = 0.1
			m.scanner.MinSpeedMBps = 0.5
			m.scanner.OnlyCMIN2 = false
			_ = m.db.SaveScanner(m.scanner)
		}
	} else {
		m.scanner = defaultScannerConfig()
		_ = m.db.SaveScanner(m.scanner)
	}

	if m.scanner.IPType == 6 && !hasIPv6Connectivity() {
		logging.WarnTo("config", "⚠ IPv6 不可用，自动切换到 IPv4")
		m.scanner.IPType = 4
		_ = m.db.SaveScanner(m.scanner)
	}

	if rs, err := m.db.LoadRegions(); err == nil && len(rs) > 0 {
		m.regions = rs
	} else {
		m.regions = []ProxyRegion{
			{Name: "香港", Code: "HKG", Port: 1001, Enabled: true, Fallback: true},
			{Name: "美国", Code: "DEN,DFW,DTW,EWR,FSD,HNL,IAD,IAH,IND,JAX,LAS,LAX,MCI,MEM,MFE,MIA,MSP,OKC,OMA,ORD,ORF,PDX,PHL,PHX,PIT,RDU,RIC,SAN,SAT,SEA,SFO,SJC,SLC,SMF,STL,TLH,TPA", Port: 1002, Enabled: true, Fallback: true},
			{Name: "日本", Code: "KIX,NRT,OKA,FUK", Port: 1003, Enabled: true, Fallback: true},
			{Name: "新加坡", Code: "SIN", Port: 1004, Enabled: true, Fallback: true},
			{Name: "越南", Code: "DAD,HAN,SGN", Port: 1005, Enabled: true, Fallback: true},
		}
		_ = m.db.SaveRegions(m.regions)
	}

	if c, err := m.db.LoadCfnat(); err == nil {
		m.cfnat = c
	} else {
		m.cfnat = defaultCfnatConfig()
		_ = m.db.SaveCfnat(m.cfnat)
	}

	if pf, err := m.db.LoadProxyForward(); err == nil {
		m.proxyForward = pf
	} else {
		m.proxyForward = defaultProxyForwardConfig()
		_ = m.db.SaveProxyForward(m.proxyForward)
	}
	return nil
}

func defaultScannerConfig() ScannerConfig {
	return ScannerConfig{
		Enabled:            false,
		Interval:           60,
		MinSpeedMBps:       0.5,
		IPType:             4,
		Port:               443,
		SamplesPer24:       100,
		MaxDelayMs:         500,
		Threads:            100,
		ScanMode:           "tcping",
		SpeedTestURL:       "auto",
		OnlyCMIN2:          false,
		CMIN2Colos:         "HKG,SIN,NRT,KIX,LAX,SJC,SEA,FRA,AMS,LHR,TPE,ICN,MNL,BKK,MFM",
		SpeedTestMode:      "speed",
		ColoAware:          true,
		MapRebuildInterval: 24,
		TargetIPsPerColo:   5,
		ExploreRatio:       0.1,
	}
}

func defaultCfnatConfig() CfnatConfig {
	return CfnatConfig{
		TLSMode:    true,
		RandomPick: true,
		IPPoolSize: 10,
		ForwardNum: 5,
		SpeedTime:  3,
		ExpectCode: 200,
	}
}

func defaultProxyForwardConfig() ProxyForwardConfig {
	return ProxyForwardConfig{
		MaxRetries:          3,
		LoadBalanceMode:     "random",
		EWMASampleWindow:    50,
		HealthCheckInterval: 120,
		MaxDelayMs:          500,
		MaxLossRate:         10,
		IsolationDuration:   300,
		WarmupDuration:      60,
		StickyEnabled:       false,
		StickyTTL:           15,
		ActivePoolSize:      20,
		StandbyPoolRatio:    0.5,
	}
}

// === 访问器 ===

func (m *Manager) General() GeneralConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.general
}

func (m *Manager) Scanner() ScannerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scanner
}

func (m *Manager) Cfnat() CfnatConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfnat
}

func (m *Manager) Regions() []ProxyRegion {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProxyRegion, len(m.regions))
	copy(out, m.regions)
	return out
}

func (m *Manager) ProxyForward() ProxyForwardConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.proxyForward
}

// === 修改器（写库 + 更新内存） ===

func (m *Manager) UpdateGeneral(g GeneralConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.SaveGeneral(g); err != nil {
		return err
	}
	m.general = g
	return nil
}

func (m *Manager) UpdateScanner(s ScannerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.SaveScanner(s); err != nil {
		return err
	}
	m.scanner = s
	return nil
}

func (m *Manager) UpdateCfnat(c CfnatConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.SaveCfnat(c); err != nil {
		return err
	}
	m.cfnat = c
	return nil
}

func (m *Manager) UpdateRegions(regions []ProxyRegion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.SaveRegions(regions); err != nil {
		return err
	}
	m.regions = regions
	return nil
}

func (m *Manager) UpdateRegion(name string, mut func(*ProxyRegion)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.regions {
		if m.regions[i].Name == name {
			mut(&m.regions[i])
			break
		}
	}
	return m.db.SaveRegions(m.regions)
}

func (m *Manager) UpdateProxyForward(pf ProxyForwardConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.SaveProxyForward(pf); err != nil {
		return err
	}
	m.proxyForward = pf
	return nil
}

// NowISO 辅助函数
func NowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// DumpJSON 用于调试
func (m *Manager) DumpJSON() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := map[string]interface{}{
		"general": m.general,
		"scanner": m.scanner,
		"regions": m.regions,
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	return string(b)
}

// Log 简单的日志包装
func (m *Manager) Logf(format string, v ...interface{}) {
	level := m.General().LogLevel
	if level == "debug" {
		log.Printf("[DEBUG] "+format, v...)
	} else {
		log.Printf(format, v...)
	}
}

func hasIPv6Connectivity() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "[2606:4700:4700::1111]:80")
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
