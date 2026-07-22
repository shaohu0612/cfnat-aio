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
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cfnat-aio/internal/logging"
)

// ProxyRegion 描述一个代理地区（监听端口 + CMIN2 IP 库）
type ProxyRegion struct {
	Name      string `json:"name"`       // HKG / LAX / JP
	Country   string `json:"country"`    // 国家/地区名称
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
	LoadBalanceMode     string  `json:"load_balance_mode"`     // 负载均衡策略：random/least-conn/weighted-random/quality-score
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
	AdaptiveThreshold   bool    `json:"adaptive_threshold"`    // 自适应阈值开关（V2.2）
	NewIPPriority       bool    `json:"new_ip_priority"`       // 新IP优先调度开关（V2.2）
	QualityThreshold    int     `json:"quality_threshold"`     // 质量淘汰阈值（V2.2）
	AutoRebuildPools    bool    `json:"auto_rebuild_pools"`    // 扫描后自动重建IP池（V2.1）
	TrendRetentionDays  int     `json:"trend_retention_days"`  // IP质量趋势数据保留天数（V2.3）
	TraceEnabled        bool    `json:"trace_enabled"`         // 全链路追踪开关（V2.3）
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
		// V2.2.1 数据迁移：补充 country 字段（旧数据无此字段）
		needMigrate := false
		for i := range m.regions {
			if m.regions[i].Country == "" {
				m.regions[i].Country = inferCountryFromRegion(m.regions[i])
				needMigrate = true
			}
		}
		if needMigrate {
			_ = m.db.SaveRegions(m.regions)
			logging.InfoTo("config", "✅ 地区 country 字段迁移完成")
		}
	} else {
		m.regions = defaultRegions()
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

// defaultRegions 默认 5 地区列表（V2.2.1）
func defaultRegions() []ProxyRegion {
	return []ProxyRegion{
		{Name: "香港", Country: "香港", Code: "HKG", Port: 1001, Enabled: true, Fallback: true},
		{Name: "美国", Country: "美国", Code: "DEN,DFW,DTW,EWR,FSD,HNL,IAD,IAH,IND,JAX,LAS,LAX,MCI,MEM,MFE,MIA,MSP,OKC,OMA,ORD,ORF,PDX,PHL,PHX,PIT,RDU,RIC,SAN,SAT,SEA,SFO,SJC,SLC,SMF,STL,TLH,TPA", Port: 1002, Enabled: true, Fallback: true},
		{Name: "日本", Country: "日本", Code: "KIX,NRT,OKA,FUK", Port: 1003, Enabled: true, Fallback: true},
		{Name: "新加坡", Country: "新加坡", Code: "SIN", Port: 1004, Enabled: true, Fallback: true},
		{Name: "越南", Country: "越南", Code: "DAD,HAN,SGN", Port: 1005, Enabled: true, Fallback: true},
	}
}

// inferCountryFromRegion 通过地区名或 colo 反查国家名（V2.2.1 迁移）
func inferCountryFromRegion(r ProxyRegion) string {
	if r.Name != "" {
		// 已知地区名直接映射
		switch r.Name {
		case "香港", "美国", "日本", "新加坡", "越南", "韩国", "台湾", "印度", "英国", "德国", "法国",
			"荷兰", "瑞典", "瑞士", "西班牙", "意大利", "俄罗斯", "巴西", "加拿大", "澳大利亚":
			return r.Name
		}
	}
	// 通过 colo 反查内置字典
	if r.Code != "" {
		for _, dc := range builtinDatacenters {
			for _, colo := range strings.Split(r.Code, ",") {
				if strings.TrimSpace(colo) == dc.Colo && dc.RegionName != "" {
					return dc.RegionName
				}
			}
		}
	}
	return r.Name
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
		AdaptiveThreshold:   false,
		NewIPPriority:       false,
		QualityThreshold:    30,
		AutoRebuildPools:    true,
		TrendRetentionDays:  7,
		TraceEnabled:        false,
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

// === 数据中心字典同步（V2.2.1）===

//go:embed cf-iata.json
var cfIATAJSON []byte

// cca2ToZhCountry CCA2 国家代码到中文名称映射
var cca2ToZhCountry = map[string]string{
	"US": "美国", "JP": "日本", "HK": "中国香港", "SG": "新加坡", "VN": "越南",
	"KR": "韩国", "GB": "英国", "DE": "德国", "FR": "法国", "NL": "荷兰",
	"AU": "澳大利亚", "CA": "加拿大", "BR": "巴西", "IN": "印度", "TW": "台湾",
	"TH": "泰国", "PH": "菲律宾", "ID": "印度尼西亚", "MY": "马来西亚",
	"MO": "澳门", "AE": "阿联酋", "SA": "沙特阿拉伯", "NZ": "新西兰",
	"CL": "智利", "MX": "墨西哥", "AR": "阿根廷", "CO": "哥伦比亚", "PE": "秘鲁",
	"ZA": "南非", "EG": "埃及", "NG": "尼日利亚", "KE": "肯尼亚", "MA": "摩洛哥",
	"SE": "瑞典", "NO": "挪威", "FI": "芬兰", "DK": "丹麦", "PL": "波兰",
	"IT": "意大利", "ES": "西班牙", "PT": "葡萄牙", "CH": "瑞士", "AT": "奥地利",
	"BE": "比利时", "IE": "爱尔兰", "RU": "俄罗斯", "UA": "乌克兰", "TR": "土耳其",
	"IL": "以色列", "JO": "约旦", "LB": "黎巴嫩", "IQ": "伊拉克", "IR": "伊朗",
	"KZ": "哈萨克斯坦", "UZ": "乌兹别克斯坦", "PK": "巴基斯坦", "BD": "孟加拉国",
	"LK": "斯里兰卡", "NP": "尼泊尔", "MM": "缅甸", "KH": "柬埔寨", "LA": "老挝",
	"BN": "文莱", "FJ": "斐济", "PG": "巴布亚新几内亚", "CK": "库克群岛",
	"EC": "厄瓜多尔", "VE": "委内瑞拉", "BO": "玻利维亚", "PY": "巴拉圭",
	"UY": "乌拉圭", "CR": "哥斯达黎加", "PA": "巴拿马", "GT": "危地马拉",
	"DO": "多米尼加", "JM": "牙买加", "BS": "巴哈马", "TT": "特立尼达和多巴哥",
	"CW": "库拉索", "AW": "阿鲁巴", "BM": "百慕大", "KY": "开曼群岛",
	"DZ": "阿尔及利亚", "TN": "突尼斯", "LY": "利比亚", "SD": "苏丹",
	"ET": "埃塞俄比亚", "TZ": "坦桑尼亚", "UG": "乌干达", "CD": "刚果民主共和国",
	"CM": "喀麦隆", "CI": "科特迪瓦", "GH": "加纳", "SN": "塞内加尔",
	"AO": "安哥拉", "MZ": "莫桑比克", "ZW": "津巴布韦", "BW": "博茨瓦纳",
	"NA": "纳米比亚", "BG": "保加利亚", "RO": "罗马尼亚", "HU": "匈牙利",
	"CZ": "捷克", "SK": "斯洛伐克", "HR": "克罗地亚", "RS": "塞尔维亚",
	"SI": "斯洛文尼亚", "EE": "爱沙尼亚", "LV": "拉脱维亚", "LT": "立陶宛",
	"IS": "冰岛", "GR": "希腊", "CY": "塞浦路斯", "MT": "马耳他", "LU": "卢森堡",
	"MC": "摩纳哥", "AD": "安道尔", "LI": "列支敦士登", "SM": "圣马力诺",
	"VA": "梵蒂冈", "BY": "白俄罗斯", "MD": "摩尔多瓦", "GE": "格鲁吉亚",
	"AM": "亚美尼亚", "AZ": "阿塞拜疆", "BH": "巴林", "QA": "卡塔尔",
	"KW": "科威特", "OM": "阿曼", "YE": "也门", "AF": "阿富汗",
	"MN": "蒙古",
}

var builtinDatacenters = []DatacenterEntry{
	{Colo: "HKG", Name: "Hong Kong", Country: "HK", City: "Hong Kong", Continent: "AS", RegionName: "香港"},
	{Colo: "SIN", Name: "Singapore", Country: "SG", City: "Singapore", Continent: "AS", RegionName: "新加坡"},
	{Colo: "NRT", Name: "Tokyo", Country: "JP", City: "Tokyo", Continent: "AS", RegionName: "日本"},
	{Colo: "KIX", Name: "Osaka", Country: "JP", City: "Osaka", Continent: "AS", RegionName: "日本"},
	{Colo: "OKA", Name: "Okinawa", Country: "JP", City: "Okinawa", Continent: "AS", RegionName: "日本"},
	{Colo: "FUK", Name: "Fukuoka", Country: "JP", City: "Fukuoka", Continent: "AS", RegionName: "日本"},
	{Colo: "DEN", Name: "Denver", Country: "US", City: "Denver", Continent: "NA", RegionName: "美国"},
	{Colo: "DFW", Name: "Dallas", Country: "US", City: "Dallas", Continent: "NA", RegionName: "美国"},
	{Colo: "LAX", Name: "Los Angeles", Country: "US", City: "Los Angeles", Continent: "NA", RegionName: "美国"},
	{Colo: "SFO", Name: "San Francisco", Country: "US", City: "San Francisco", Continent: "NA", RegionName: "美国"},
	{Colo: "SEA", Name: "Seattle", Country: "US", City: "Seattle", Continent: "NA", RegionName: "美国"},
	{Colo: "SJC", Name: "San Jose", Country: "US", City: "San Jose", Continent: "NA", RegionName: "美国"},
	{Colo: "EWR", Name: "Newark", Country: "US", City: "Newark", Continent: "NA", RegionName: "美国"},
	{Colo: "ORD", Name: "Chicago", Country: "US", City: "Chicago", Continent: "NA", RegionName: "美国"},
	{Colo: "PHL", Name: "Philadelphia", Country: "US", City: "Philadelphia", Continent: "NA", RegionName: "美国"},
	{Colo: "IAD", Name: "Washington", Country: "US", City: "Washington", Continent: "NA", RegionName: "美国"},
	{Colo: "MIA", Name: "Miami", Country: "US", City: "Miami", Continent: "NA", RegionName: "美国"},
	{Colo: "PHX", Name: "Phoenix", Country: "US", City: "Phoenix", Continent: "NA", RegionName: "美国"},
	{Colo: "LAS", Name: "Las Vegas", Country: "US", City: "Las Vegas", Continent: "NA", RegionName: "美国"},
	{Colo: "PDX", Name: "Portland", Country: "US", City: "Portland", Continent: "NA", RegionName: "美国"},
	{Colo: "MEM", Name: "Memphis", Country: "US", City: "Memphis", Continent: "NA", RegionName: "美国"},
	{Colo: "IND", Name: "Indianapolis", Country: "US", City: "Indianapolis", Continent: "NA", RegionName: "美国"},
	{Colo: "JAX", Name: "Jacksonville", Country: "US", City: "Jacksonville", Continent: "NA", RegionName: "美国"},
	{Colo: "DTW", Name: "Detroit", Country: "US", City: "Detroit", Continent: "NA", RegionName: "美国"},
	{Colo: "FSD", Name: "Sioux Falls", Country: "US", City: "Sioux Falls", Continent: "NA", RegionName: "美国"},
	{Colo: "HNL", Name: "Honolulu", Country: "US", City: "Honolulu", Continent: "NA", RegionName: "美国"},
	{Colo: "IAH", Name: "Houston", Country: "US", City: "Houston", Continent: "NA", RegionName: "美国"},
	{Colo: "MCI", Name: "Kansas City", Country: "US", City: "Kansas City", Continent: "NA", RegionName: "美国"},
	{Colo: "MFE", Name: "McAllen", Country: "US", City: "McAllen", Continent: "NA", RegionName: "美国"},
	{Colo: "MSP", Name: "Minneapolis", Country: "US", City: "Minneapolis", Continent: "NA", RegionName: "美国"},
	{Colo: "OKC", Name: "Oklahoma City", Country: "US", City: "Oklahoma City", Continent: "NA", RegionName: "美国"},
	{Colo: "OMA", Name: "Omaha", Country: "US", City: "Omaha", Continent: "NA", RegionName: "美国"},
	{Colo: "ORF", Name: "Norfolk", Country: "US", City: "Norfolk", Continent: "NA", RegionName: "美国"},
	{Colo: "PIT", Name: "Pittsburgh", Country: "US", City: "Pittsburgh", Continent: "NA", RegionName: "美国"},
	{Colo: "RDU", Name: "Raleigh", Country: "US", City: "Raleigh", Continent: "NA", RegionName: "美国"},
	{Colo: "RIC", Name: "Richmond", Country: "US", City: "Richmond", Continent: "NA", RegionName: "美国"},
	{Colo: "SAN", Name: "San Diego", Country: "US", City: "San Diego", Continent: "NA", RegionName: "美国"},
	{Colo: "SAT", Name: "San Antonio", Country: "US", City: "San Antonio", Continent: "NA", RegionName: "美国"},
	{Colo: "SLC", Name: "Salt Lake City", Country: "US", City: "Salt Lake City", Continent: "NA", RegionName: "美国"},
	{Colo: "SMF", Name: "Sacramento", Country: "US", City: "Sacramento", Continent: "NA", RegionName: "美国"},
	{Colo: "STL", Name: "St. Louis", Country: "US", City: "St. Louis", Continent: "NA", RegionName: "美国"},
	{Colo: "TLH", Name: "Tallahassee", Country: "US", City: "Tallahassee", Continent: "NA", RegionName: "美国"},
	{Colo: "TPA", Name: "Tampa", Country: "US", City: "Tampa", Continent: "NA", RegionName: "美国"},
	{Colo: "DAD", Name: "Da Nang", Country: "VN", City: "Da Nang", Continent: "AS", RegionName: "越南"},
	{Colo: "HAN", Name: "Hanoi", Country: "VN", City: "Hanoi", Continent: "AS", RegionName: "越南"},
	{Colo: "SGN", Name: "Ho Chi Minh", Country: "VN", City: "Ho Chi Minh", Continent: "AS", RegionName: "越南"},
	{Colo: "TPE", Name: "Taipei", Country: "TW", City: "Taipei", Continent: "AS", RegionName: "台湾"},
	{Colo: "ICN", Name: "Seoul", Country: "KR", City: "Seoul", Continent: "AS", RegionName: "韩国"},
	{Colo: "MNL", Name: "Manila", Country: "PH", City: "Manila", Continent: "AS", RegionName: "菲律宾"},
	{Colo: "BKK", Name: "Bangkok", Country: "TH", City: "Bangkok", Continent: "AS", RegionName: "泰国"},
	{Colo: "FRA", Name: "Frankfurt", Country: "DE", City: "Frankfurt", Continent: "EU", RegionName: "德国"},
	{Colo: "AMS", Name: "Amsterdam", Country: "NL", City: "Amsterdam", Continent: "EU", RegionName: "荷兰"},
	{Colo: "LHR", Name: "London", Country: "GB", City: "London", Continent: "EU", RegionName: "英国"},
	{Colo: "MFM", Name: "Macau", Country: "MO", City: "Macau", Continent: "AS", RegionName: "澳门"},
	{Colo: "DXB", Name: "Dubai", Country: "AE", City: "Dubai", Continent: "AS", RegionName: "阿联酋"},
	{Colo: "SYD", Name: "Sydney", Country: "AU", City: "Sydney", Continent: "OC", RegionName: "澳大利亚"},
	{Colo: "AKL", Name: "Auckland", Country: "NZ", City: "Auckland", Continent: "OC", RegionName: "新西兰"},
	{Colo: "SCL", Name: "Santiago", Country: "CL", City: "Santiago", Continent: "SA", RegionName: "智利"},
	{Colo: "GRU", Name: "Sao Paulo", Country: "BR", City: "Sao Paulo", Continent: "SA", RegionName: "巴西"},
	{Colo: "JNB", Name: "Johannesburg", Country: "ZA", City: "Johannesburg", Continent: "AF", RegionName: "南非"},
	{Colo: "CAI", Name: "Cairo", Country: "EG", City: "Cairo", Continent: "AF", RegionName: "埃及"},
}

func (m *Manager) InitDatacenters() {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		count := 0
		if rows, err := sqliteStore.db.Query(`SELECT COUNT(*) FROM cf_datacenter`); err == nil {
			if rows.Next() {
				_ = rows.Scan(&count)
			}
			rows.Close()
		}

		// 始终 upsert 内置字典，确保新增条目被写入（已有条目保持不变）
		if count == 0 {
			logging.InfoTo("config", "📦 初始化内置数据中心字典")
		} else {
			logging.InfoTo("config", "📦 同步内置数据中心字典（当前 %d 条）", count)
		}
		for i := range builtinDatacenters {
			builtinDatacenters[i].UpdatedAt = NowISO()
		}
		if err := sqliteStore.UpsertDatacentersBatch(builtinDatacenters); err != nil {
			logging.ErrorTo("config", "数据中心字典初始化失败: %v", err)
		}
	}
}

func (m *Manager) SyncDatacenters() error {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		// 优先尝试远程拉取，失败则使用内置 cf-iata.json
		var body []byte
		remoteOK := false

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", "https://raw.githubusercontent.com/cloudflare/cf-speedtest/master/servers.json", nil)
		if err == nil {
			client := &http.Client{}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					body, err = io.ReadAll(resp.Body)
					if err == nil {
						remoteOK = true
						logging.InfoTo("config", "数据中心字典远程拉取成功")
					}
				}
			}
		}

		if !remoteOK {
			logging.WarnTo("config", "远程数据中心字典拉取失败，使用内置 cf-iata.json")
			body = cfIATAJSON
		}

		// cf-iata.json 格式：{ "IATA": { "place":"...", "place_zh":"...", "lat":.., "lng":.., "cca2":"..", "region":".." } }
		var rawMap map[string]struct {
			Place    string  `json:"place"`
			PlaceZh  string  `json:"place_zh"`
			Lat      float64 `json:"lat"`
			Lng      float64 `json:"lng"`
			CCA2     string  `json:"cca2"`
			Region   string  `json:"region"`
		}
		if err := json.Unmarshal(body, &rawMap); err != nil {
			// 可能是旧格式（数组），尝试旧解析
			type remoteDC struct {
				Code      string  `json:"code"`
				City      string  `json:"city"`
				Country   string  `json:"country"`
				Region    string  `json:"region"`
				Latitude  float64 `json:"lat"`
				Longitude float64 `json:"lon"`
			}
			var remoteDCs []remoteDC
			if err2 := json.Unmarshal(body, &remoteDCs); err2 != nil {
				logging.WarnTo("config", "数据中心同步解析失败: %v / %v", err, err2)
				return err
			}
			// 旧格式处理
			now := NowISO()
			var entries []DatacenterEntry
			for _, dc := range remoteDCs {
				entries = append(entries, DatacenterEntry{
					Colo:       dc.Code,
					Name:       dc.City,
					Country:    dc.Country,
					City:       dc.City,
					Latitude:   dc.Latitude,
					Longitude:  dc.Longitude,
					RegionName: cca2ToZhCountry[dc.Country],
					UpdatedAt:  now,
				})
			}
			if len(entries) > 0 {
				if err := sqliteStore.UpsertDatacentersBatch(entries); err != nil {
					return err
				}
				_ = sqliteStore.SetDatacenterMeta("last_sync", now)
				logging.InfoTo("config", "✅ 数据中心字典同步完成: %d 个数据中心", len(entries))
			}
			return nil
		}

		// cf-iata.json 格式处理
		now := NowISO()
		var entries []DatacenterEntry
		for colo, dc := range rawMap {
			regionName := cca2ToZhCountry[dc.CCA2]
			if regionName == "" {
				regionName = dc.Region
			}
			entries = append(entries, DatacenterEntry{
				Colo:       colo,
				Name:       dc.PlaceZh,
				Country:    dc.CCA2,
				City:       dc.PlaceZh,
				Continent:  dc.Region,
				Latitude:   dc.Lat,
				Longitude:  dc.Lng,
				RegionName: regionName,
				UpdatedAt:  now,
			})
		}

		if len(entries) > 0 {
			if err := sqliteStore.UpsertDatacentersBatch(entries); err != nil {
				logging.ErrorTo("config", "数据中心同步写入失败: %v", err)
				return err
			}
			logging.InfoTo("config", "✅ 数据中心字典同步完成: %d 个数据中心", len(entries))
			_ = sqliteStore.SetDatacenterMeta("last_sync", now)
		}
	}
	return nil
}

func (m *Manager) ListDatacenters() ([]DatacenterEntry, error) {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		return sqliteStore.ListDatacenters()
	}
	return nil, fmt.Errorf("not supported")
}

func (m *Manager) ListDatacentersByCountry(country string) ([]DatacenterEntry, error) {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		return sqliteStore.ListDatacentersByCountry(country)
	}
	return nil, fmt.Errorf("not supported")
}

func (m *Manager) ListCountries() ([]string, error) {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		return sqliteStore.ListCountries()
	}
	return nil, fmt.Errorf("not supported")
}

func (m *Manager) GetDatacenterMeta(key string) (string, error) {
	if sqliteStore, ok := m.db.(*SQLiteStore); ok {
		return sqliteStore.GetDatacenterMeta(key)
	}
	return "", fmt.Errorf("not supported")
}
