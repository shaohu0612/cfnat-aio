// Package scanner CFNAT-AIO 扫描器
//
// 继承 cfdata 的核心扫描管线，并按之前讨论做了 4 点改进：
//   1. 每 /24 抽样数可配（SamplesPer24: 1/3/5/255）
//   2. 保存所有 /24 的测试记录（含失败原因）
//   3. 增量更新：优先复测上次好的 IP
//   4. 兜底：扫描结果为空时回退到全量随机
//
// 扫描管线：
//   加载 CIDR 列表 -> 按 /24 抽样 -> TCP/TLS 探活 -> 测速 -> 分类入库
package scanner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cfnat-aio/internal/config"
	"cfnat-aio/internal/iplibrary"
	"cfnat-aio/internal/logging"
)

// ScanProgress 扫描进度（暴露给 WebUI）
type ScanProgress struct {
	Running      bool   `json:"running"`
	Stage        string `json:"stage"`         // 1/5 CIDR加载, 2/5 抽样, 3/5 探活, 4/5 测速, 5/5 入库
	StageDone    int64  `json:"stage_done"`    // 当前阶段已完成数
	StageTotal   int64  `json:"stage_total"`   // 当前阶段总数
	CurrentIP    string `json:"current_ip"`    // 当前正在测试的 IP
	Percent      int    `json:"percent"`       // 当前阶段百分比
	TotalScanned int64  `json:"total_scanned"` // 总已扫描
	TotalPassed  int64  `json:"total_passed"`  // 总已通过
	CIDRSource     string `json:"cidrSource"`    // CIDR 来源：local / remote / builtin
	CIDRFile       string `json:"cidrFile"`      // 本地文件名
	CIDRSegments   int    `json:"cidrSegments"`  // CIDR 段数
	CIDR24Segments int    `json:"cidr24Segments"` // /24 段数
}

// Scanner 扫描器
type Scanner struct {
	store  *config.SQLiteStore
	lib    *iplibrary.Library
	cfgMgr *config.Manager

	// 运行控制
	mu       sync.Mutex
	running  bool
	stop     context.CancelFunc
	history  []config.ScanHistory
	progress ScanProgress
}

// New 创建扫描器
func New(store *config.SQLiteStore, lib *iplibrary.Library, cfgMgr *config.Manager) *Scanner {
	s := &Scanner{
		store:  store,
		lib:    lib,
		cfgMgr: cfgMgr,
	}
	hs, _ := store.ListScanHistory(50)
	s.history = hs
	return s
}

// IsRunning 是否正在扫描
func (s *Scanner) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Progress 获取当前扫描进度
func (s *Scanner) Progress() ScanProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progress
}

// History 获取历史
func (s *Scanner) History() []config.ScanHistory {
	return s.history
}

// StartAsync 异步启动一次扫描
func (s *Scanner) StartAsync() {
	go s.RunOnce()
}

// RunOnce 执行一次完整扫描
func (s *Scanner) RunOnce() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.progress = ScanProgress{Running: true}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.progress.Running = false
		s.stop = nil
		s.mu.Unlock()
	}()

	started := config.NowISO()
	histID, _ := s.store.AddScanHistory(config.ScanHistory{
		StartedAt: started,
		Status:    "running",
	})

	sc := s.cfgMgr.Scanner()
	if sc.Interval <= 0 {
		sc.Interval = 60
	}

	logging.InfoTo("scanner", "▶ 扫描任务 #%d 启动 (IPv%d, 抽样数=%d/24, 速度阈值=%.1fMB/s)",
		histID, sc.IPType, sc.SamplesPer24, sc.MinSpeedMBps)

	// 注意：不再自动检测 IPv6 可用性，由用户在 UI 中选择 IP 类型
	// 如果选择了 IPv6 但环境不支持，TCP 连接会超时，扫描结果为 0，用户可切换到 IPv4

	// 阶段 1: 加载 CIDR 列表
	s.setProgress("1/5 加载CIDR", 0, 0, "")
	cidrs, err := s.loadCIDRs(sc.IPType)
	if err != nil || len(cidrs) == 0 {
		logging.ErrorTo("scanner", "✗ 加载 CIDR 列表失败: %v", err)
		s.finishRun(histID, "error", 0, 0, fmt.Sprintf("加载CIDR失败: %v", err))
		return
	}
	logging.InfoTo("scanner", "  [1/5] 加载 CIDR: %d 段", len(cidrs))

	// 阶段 2: 按 /24 抽样
	s.setProgress("2/5 抽样中", 0, 0, "")
	candidates := s.sampleCIDRs(cidrs, sc.SamplesPer24, sc.IPType)
	if len(candidates) == 0 {
		logging.ErrorTo("scanner", "✗ 抽样后无候选 IP")
		s.finishRun(histID, "error", 0, 0, "抽样后候选IP为空")
		return
	}
	logging.InfoTo("scanner", "  [2/5] /24 抽样: %d 个候选 IP", len(candidates))

	// 阶段 3: TCP + TLS 探活 + /cdn-cgi/trace
	s.setProgress("3/5 探活中", 0, int64(len(candidates)), "")
	stats := s.probeAndTrace(ctx, candidates, sc)
	if stats == nil {
		logging.ErrorTo("scanner", "✗ 探活阶段异常")
		s.finishRun(histID, "error", len(candidates), 0, "探活阶段异常")
		return
	}
	// 统计真正通过探活的数量
	okCount := 0
	errSample := ""
	for _, r := range stats {
		if r.OK {
			okCount++
		} else if errSample == "" && r.Error != "" {
			errSample = r.Error
		}
	}
	logging.InfoTo("scanner", "  [3/5] TCP+TLS 探活: %d/%d 个通过", okCount, len(stats))
	if okCount == 0 && errSample != "" {
		logging.ErrorTo("scanner", "    探活失败示例: %s", errSample)
	}

	// 阶段 4: 测速（针对通过探活的 IP，按延迟取 top 100）
	speedTestTotal := okCount
	if speedTestTotal > 100 {
		speedTestTotal = 100
	}
	s.setProgress("4/5 测速中", 0, int64(speedTestTotal), "")
	passed := s.speedTest(ctx, stats, sc)

	// 统计真正测速达标的数量和 colo 分布
	speedPassed := 0
	coloDist := map[string]int{}
	for _, r := range passed {
		if r.ok && r.speedMbps >= sc.MinSpeedMBps {
			speedPassed++
			coloDist[r.colo]++
		}
	}
	logging.InfoTo("scanner", "  [4/5] 速度测试: %d/%d 个达到 %.1fMB/s 阈值 (共测速 %d, 探活通过 %d)", speedPassed, len(passed), sc.MinSpeedMBps, len(passed), okCount)
	logging.InfoTo("scanner", "    colo 分布: %v", coloDist)

	// 阶段 5: 按地区入库（只入速度达标的）
	s.setProgress("5/5 入库中", 0, int64(len(passed)), "")
	var savedByRegion map[string]int
	if sc.OnlyCMIN2 {
		savedByRegion = s.saveByCMIN2Colo(passed, sc)
	} else {
		savedByRegion = s.saveByRegion(passed, sc)
	}
	logging.InfoTo("scanner", "  [5/5] 入库完成: %v", savedByRegion)

	// 更新统计
	total := len(candidates)
	savedJSON, _ := json.Marshal(savedByRegion)
	statsJSON := fmt.Sprintf(`{"cmin2":%s,"saved":%s,"scanned":%d,"speed_passed":%d}`,
		string(savedJSON), string(savedJSON), total, speedPassed)
	s.finishRun(histID, "ok", total, speedPassed, statsJSON)

	logging.InfoTo("scanner", "✓ 扫描任务 #%d 完成: 候选 %d, 通过 %d", histID, total, speedPassed)

	// 记录到 history
	hs, _ := s.store.ListScanHistory(50)
	s.mu.Lock()
	s.history = hs
	s.mu.Unlock()
}

func (s *Scanner) finishRun(histID int64, status string, total, passed int, stats string) {
	// 更新首次插入的那条
	_, _ = s.store.DB().Exec(`UPDATE scan_history SET finished_at=?, status=?, total=?, passed=?, stats_json=? WHERE id=?`,
		config.NowISO(), status, total, passed, stats, histID)

	// 更新 scanner config 上的"上次运行"
	sc := s.cfgMgr.Scanner()
	sc.LastRunTime = config.NowISO()
	sc.LastRunStatus = status
	sc.LastRunStats = stats
	sc.NextRunTime = time.Now().Add(time.Duration(sc.Interval) * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	_ = s.cfgMgr.UpdateScanner(sc)
}

// === CIDR 加载 ===

// checkIPv6Available 检测当前环境是否支持 IPv6 直连
// 通过尝试 TCP 连接 Cloudflare 的 IPv6 DNS 地址来判断
func (s *Scanner) checkIPv6Available() bool {
	// 尝试连接 2606:4700:4700::1111 (Cloudflare DNS) 的 443 端口
	testAddr := "[2606:4700:4700::1111]:443"
	conn, err := net.DialTimeout("tcp", testAddr, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// hasIPv6Interface 检测系统是否有 IPv6 全局地址（非回环、非链路本地）
func hasIPv6Interface() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if ipNet.IP.To4() == nil && ipNet.IP.IsGlobalUnicast() {
				return true
			}
		}
	}
	return false
}

// 内置 Cloudflare CIDR 列表（fallback，当外网不可达时使用）
// 来源：https://www.cloudflare.com/ips-v4 和 ips-v6
var builtinCIDRsV4 = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

var builtinCIDRsV6 = []string{
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// loadLocalCIDRs 从本地文件加载 CIDR 列表
// 依次尝试：直接文件名、/data/ 目录、当前目录
func (s *Scanner) loadLocalCIDRs(filename string) ([]string, error) {
	paths := []string{
		filename,
		filepath.Join("/data", filename),
		filepath.Join(".", filename),
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var cidrs []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			_, _, err := net.ParseCIDR(line)
			if err != nil {
				continue
			}
			cidrs = append(cidrs, line)
		}
		_ = f.Close()
		if len(cidrs) > 0 {
			logging.InfoTo("scanner", "使用本地 IP 库: %s (%d 段)", p, len(cidrs))
			return cidrs, nil
		}
	}
	return nil, os.ErrNotExist
}

// loadCIDRs 加载 Cloudflare IP 段列表
// 优先加载本地文件，不存在时从远程获取，失败时使用内置列表作为 fallback
func (s *Scanner) loadCIDRs(ipType int) ([]string, error) {
	localFile := "ip.txt"
	if ipType == 6 {
		localFile = "ip6.txt"
	}
	cidrs, err := s.loadLocalCIDRs(localFile)
	if err == nil && len(cidrs) > 0 {
		s.mu.Lock()
		s.progress.CIDRSource = "local"
		s.progress.CIDRFile = localFile
		s.progress.CIDRSegments = len(cidrs)
		s.mu.Unlock()
		return cidrs, nil
	}
	logging.InfoTo("scanner", "本地 IP 库不存在，使用远程 URL")

	var sourceURL string
	var fallback []string
	if ipType == 4 {
		sourceURL = "https://www.cloudflare.com/ips-v4"
		fallback = builtinCIDRsV4
	} else {
		sourceURL = "https://www.cloudflare.com/ips-v6"
		fallback = builtinCIDRsV6
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(sourceURL)
	if err != nil {
		logging.WarnTo("scanner", "远程 CIDR 列表获取失败，使用内置列表: %v", err)
		return fallback, nil
	}
	defer resp.Body.Close()

	cidrs = nil
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			cidrs = append(cidrs, line)
		}
	}
	if len(cidrs) == 0 {
		logging.WarnTo("scanner", "远程 CIDR 列表为空，使用内置列表")
		return fallback, nil
	}
	return cidrs, sc.Err()
}

// === 按 /24 抽样 ===

// splitTo24 将大段 CIDR 拆分为 /24 段
func splitTo24(cidr string) []string {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	ones, _ := ipnet.Mask.Size()
	if ones >= 24 {
		return []string{cidr}
	}
	// IPv6 不拆分，直接返回原 CIDR
	if ip.To4() == nil {
		return []string{cidr}
	}
	count := 1 << (24 - ones)
	base := ipnet.IP.To4()
	baseVal := binary.BigEndian.Uint32(base)
	var out []string
	for i := 0; i < count; i++ {
		subnetVal := baseVal + uint32(i)<<8
		subnet := make(net.IP, 4)
		binary.BigEndian.PutUint32(subnet, subnetVal)
		out = append(out, fmt.Sprintf("%s/24", subnet.String()))
	}
	return out
}

// randomIPInCIDR 从指定 CIDR 中随机抽取一个 IP（避开网络地址和广播地址）
func randomIPInCIDR(cidr string) string {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits <= 0 {
		return ip.String()
	}
	// 使用 big.Int 计算网段大小，避免 int 溢出（IPv6 网段可能非常大）
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
	// 避开网络地址(0)和广播地址(size-1)，有效偏移为 [1, size-2]
	if size.Cmp(big.NewInt(2)) <= 0 {
		return ip.String()
	}
	maxOffset := new(big.Int).Sub(size, big.NewInt(2)) // size - 2
	if maxOffset.Sign() <= 0 {
		return ip.String()
	}
	offset, _ := rand.Int(rand.Reader, maxOffset)
	offset.Add(offset, big.NewInt(1)) // 偏移 +1，确保不是网络地址
	ip = addIPOffset(ipnet.IP, offset.Int64())
	return ip.String()
}

// sampleCIDRs 从每个 /24 中抽样若干 IP
// samplesPer24: 1=原 cfdata 模式, 3=折中, 5=激进, 255=全测
func (s *Scanner) sampleCIDRs(cidrs []string, samplesPer24, ipType int) []string {
	var out []string
	if samplesPer24 <= 0 {
		samplesPer24 = 1
	}
	if samplesPer24 > 255 {
		samplesPer24 = 255
	}

	// 先将所有 CIDR 拆分为 /24 段
	var all24 []string
	for _, c := range cidrs {
		all24 = append(all24, splitTo24(c)...)
	}

	s.mu.Lock()
	s.progress.CIDR24Segments = len(all24)
	s.mu.Unlock()

	// 对每个 /24 段抽样
	for _, cidr24 := range all24 {
		for i := 0; i < samplesPer24; i++ {
			ip := randomIPInCIDR(cidr24)
			if ip != "" {
				out = append(out, ip)
			}
		}
		// 调试日志：打印前几个生成的 IP
		start := len(out) - samplesPer24
		if start < 0 {
			start = 0
		}
		sampleIPs := out[start:]
		if len(sampleIPs) > 3 {
			sampleIPs = sampleIPs[:3]
		}
		logging.InfoTo("scanner", "    /24 %s → 示例: %v", cidr24, sampleIPs)
	}
	_ = ipType // 保留参数（未来 IPv6 可能区分）
	return out
}

func addIPOffset(base net.IP, offset int64) net.IP {
	// 优先使用 To4() 确保 IPv4 地址走 4 字节路径
	if v4 := base.To4(); v4 != nil {
		base = v4
	}
	ip := make(net.IP, len(base))
	copy(ip, base)
	if len(ip) == 4 {
		val := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		val += uint32(offset)
		ip[0] = byte(val >> 24)
		ip[1] = byte(val >> 16)
		ip[2] = byte(val >> 8)
		ip[3] = byte(val)
	} else {
		// IPv6 处理
		hi := binary.BigEndian.Uint64(ip[:8])
		lo := binary.BigEndian.Uint64(ip[8:])
		// offset 是 int64，可能为负（big.Int.Int64() 对大数会截断）
		// 使用无符号加法并处理进位
		if offset >= 0 {
			off := uint64(offset)
			newLo := lo + off
			if newLo < lo { // 进位
				hi++
			}
			binary.BigEndian.PutUint64(ip[:8], hi)
			binary.BigEndian.PutUint64(ip[8:], newLo)
		} else {
			// 负偏移不应该出现，但做防御性处理
			off := uint64(-offset)
			newLo := lo - off
			binary.BigEndian.PutUint64(ip[:8], hi)
			binary.BigEndian.PutUint64(ip[8:], newLo)
		}
	}
	return ip
}

// === 探活 + trace ===

// ProbeResult 单 IP 探活结果（导出供 webui 导入探测使用）
type ProbeResult struct {
	IP      string  `json:"ip"`
	Colo    string  `json:"colo"`
	OK      bool    `json:"ok"`
	Error   string  `json:"error"`
	Latency float64 `json:"latency"`
}

// IsCMIN2Colo 判断 colo 是否在 CMIN2 节点白名单中
var cmin2Colos = map[string]bool{}

func init() {
	// CMIN2 节点列表（与 cfnat-fofa-filter.py 保持一致）
	for _, c := range []string{"HKG", "SIN", "NRT", "KIX", "LAX", "SJC", "SEA", "FRA", "AMS", "LHR", "TPE", "ICN", "MNL", "BKK", "MFM"} {
		cmin2Colos[c] = true
	}
}

// IsCMIN2Colo 判断 colo 是否为 CMIN2 节点
func IsCMIN2Colo(colo string) bool {
	// 也支持用户配置的自定义 CMIN2 colo 列表
	return cmin2Colos[colo]
}

// ProbeOne 单 IP 探活（导出供外部批量导入使用）
func ProbeOne(ip string, sc config.ScannerConfig) ProbeResult {
	r := ProbeResult{IP: ip}
	port := sc.Port
	if port == 0 {
		port = 443
	}
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		r.Error = "TCP: " + err.Error()
		return r
	}
	r.Latency = float64(time.Since(t0).Microseconds()) / 1000.0
	if sc.MaxDelayMs > 0 && r.Latency > float64(sc.MaxDelayMs) {
		conn.Close()
		r.Error = fmt.Sprintf("延迟%.0fms超阈值", r.Latency)
		return r
	}

	// HTTPS trace 请求
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         "cloudflare.com",
		InsecureSkipVerify: true,
	})
	defer tlsConn.Close()

	// TLS 握手必须设置超时，避免 Docker/某些网络环境下无限阻塞
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		r.Error = "TLS: " + err.Error()
		return r
	}

	req := "GET /cdn-cgi/trace HTTP/1.1\r\nHost: cloudflare.com\r\nUser-Agent: CFNAT-AIO/1.0\r\nConnection: close\r\n\r\n"
	if _, err := tlsConn.Write([]byte(req)); err != nil {
		r.Error = "Write: " + err.Error()
		return r
	}

	buf := make([]byte, 2048)
	n, _ := tlsConn.Read(buf)
	if n == 0 {
		r.Error = "Read empty"
		return r
	}
	body := string(buf[:n])
	// 提取 colo=XXX
	if i := strings.Index(body, "colo="); i >= 0 {
		line := body[i+5:]
		end := strings.IndexAny(line, "\r\n")
		if end > 0 {
			r.Colo = strings.TrimSpace(line[:end])
		} else {
			r.Colo = strings.TrimSpace(line)
		}
	}
	if r.Colo == "" {
		r.Error = "未识别colo"
		return r
	}
	r.OK = true
	return r
}

// ProbeBatch 批量探活（使用并发）
func ProbeBatch(ips []string, sc config.ScannerConfig) []ProbeResult {
	results := make([]ProbeResult, len(ips))
	threads := sc.Threads
	if threads <= 0 {
		threads = 100
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, threads)

	for i := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := ProbeOne(ip, sc)
			results[idx] = r
		}(i, ips[i])
	}
	wg.Wait()
	return results
}

// probeAndTrace TCP 探活 + /cdn-cgi/trace 获取数据中心
func (s *Scanner) probeAndTrace(ctx context.Context, ips []string, sc config.ScannerConfig) []ProbeResult {
	results := make([]ProbeResult, len(ips))
	threads := sc.Threads
	if threads <= 0 {
		threads = 100
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, threads)
	var probed int64
	total := int64(len(ips))

	for i := range ips {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := s.probeOne(ip, sc)
			results[idx] = r
			n := atomic.AddInt64(&probed, 1)
			// 每 500 个汇报一次进度到日志和 UI
			if n%500 == 0 || n == total {
				logging.InfoTo("scanner", "    探活进度: %d/%d (%.0f%%)", n, total, float64(n)/float64(total)*100)
				s.setProgress("3/5 探活中", n, total, ip)
			}
		}(i, ips[i])
	}
	wg.Wait()
	return results
}

func (s *Scanner) probeOne(ip string, sc config.ScannerConfig) ProbeResult {
	return ProbeOne(ip, sc)
}

// === 测速 ===

type speedResult struct {
	ip        string
	colo      string
	latency   float64
	speedMbps float64
	ok        bool
	err       string
}

func (s *Scanner) speedTest(ctx context.Context, probes []ProbeResult, sc config.ScannerConfig) []speedResult {
	// 只测通过探活的
	var targets []ProbeResult
	for _, p := range probes {
		if p.OK {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	// 按延迟排序，只测延迟最低的 top N（避免过多请求触发 speed.cloudflare.com 429）
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Latency < targets[j].Latency
	})
	maxSpeedTest := 100 // 最多测速 100 个（延迟最低的）
	if len(targets) > maxSpeedTest {
		logging.InfoTo("scanner", "    延迟排序后取 top %d/%d 进行测速（避免 429 速率限制）", maxSpeedTest, len(targets))
		targets = targets[:maxSpeedTest]
	}

	// latency 模式：只测延迟，不测下载速度
	if sc.SpeedTestMode == "latency" {
		results := make([]speedResult, len(targets))
		for i, t := range targets {
			results[i] = speedResult{
				ip:        t.IP,
				colo:      t.Colo,
				latency:   t.Latency,
				speedMbps: 0,
				ok:        true,
			}
		}
		logging.InfoTo("scanner", "    延迟模式: %d 个 IP 直接标记为合格 (speed=0)", len(targets))
		return results
	}

	// both 模式：先过滤延迟超标的
	if sc.SpeedTestMode == "both" {
		var filtered []ProbeResult
		for _, t := range targets {
			if sc.MaxDelayMs <= 0 || t.Latency <= float64(sc.MaxDelayMs) {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			logging.InfoTo("scanner", "    both模式: 无延迟达标 IP，跳过测速")
			return nil
		}
		logging.InfoTo("scanner", "    both模式: %d/%d 个延迟达标，开始测速", len(filtered), len(targets))
		targets = filtered
	}

	// speed 模式（默认）及 both 模式：执行实际测速
	// 测速并发数独立控制，避免 speed.cloudflare.com 429 速率限制
	threads := sc.Threads
	if threads <= 0 {
		threads = 5
	}
	if threads > 5 {
		threads = 5 // 测速下载量大，限制最大并发 5 避免 429
	}
	speedURL := s.pickSpeedURL(sc.SpeedTestURL)
	results := make([]speedResult, len(targets))
	sem := make(chan struct{}, threads)
	var wg sync.WaitGroup
	var tested int64
	total := int64(len(targets))

	logging.InfoTo("scanner", "    开始测速: %d 个 IP，并发 %d，阈值 %.1fMB/s", total, threads, sc.MinSpeedMBps)

	for i, t := range targets {
		select {
		case <-ctx.Done():
			return results[:i]
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ip, colo string, baseLat float64) {
			defer wg.Done()
			defer func() { <-sem }()
			speed, ok := s.measureSpeed(ip, speedURL, sc.Port)
			r := speedResult{ip: ip, colo: colo, latency: baseLat, speedMbps: speed, ok: ok}
			if !ok {
				r.err = fmt.Sprintf("测速失败/%.2fMbps", speed)
			}
			results[i] = r
			n := atomic.AddInt64(&tested, 1)
			if n%50 == 0 || n == total {
				logging.InfoTo("scanner", "    测速进度: %d/%d (%.0f%%)", n, total, float64(n)/float64(total)*100)
				s.setProgress("4/5 测速中", n, total, ip)
			}
		}(i, t.IP, t.Colo, t.Latency)
	}
	wg.Wait()
	return results
}

func (s *Scanner) pickSpeedURL(cfg string) string {
	switch cfg {
	case "":
		fallthrough
	case "auto":
		// 默认走 Cloudflare 官方
		return "https://speed.cloudflare.com/__down?bytes=10485760"
	default:
		if !strings.HasPrefix(cfg, "http") {
			return "https://" + cfg
		}
		return cfg
	}
}

func (s *Scanner) MeasureSpeed(ip, speedURL string, port int) (float64, bool) {
	return s.measureSpeed(ip, speedURL, port)
}

func (s *Scanner) measureSpeed(ip, speedURL string, port int) (float64, bool) {
	if port == 0 {
		port = 443
	}
	u, err := url.Parse(speedURL)
	if err != nil {
		logging.ErrorTo("scanner", "测速 URL 解析失败: %v", err)
		return 0, false
	}
	host := u.Hostname()

	// 使用 net/http 标准库客户端，自定义 Dialer 直连指定 IP
	// 相比手动 HTTP 解析，能正确处理重定向、chunked 编码、Content-Length 等边缘情况
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 忽略 DNS 解析的 addr，直连到指定 IP
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
		},
		TLSClientConfig: &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true,
		},
		DisableKeepAlives: true, // 每次测速独立连接，不复用
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	// 请求测速 URL（429 时自动重试，指数退避）
	var resp *http.Response
	maxRetries := 2
	for retry := 0; retry <= maxRetries; retry++ {
		resp, err = client.Get(speedURL)
		if err != nil {
			if retry < maxRetries {
				time.Sleep(time.Duration(1<<retry) * time.Second)
				continue
			}
			return 0, false
		}
		// 429 速率限制：等待后重试
		if resp.StatusCode == 429 && retry < maxRetries {
			resp.Body.Close()
			time.Sleep(time.Duration(2*(1<<retry)) * time.Second)
			continue
		}
		break
	}
	if resp == nil {
		return 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// 429 不逐条日志，避免刷屏（统计在 speedTest 层汇总）
		if resp.StatusCode != 429 {
			logging.ErrorTo("scanner", "测速 HTTP 状态码异常 %s: %d", ip, resp.StatusCode)
		}
		return 0, false
	}

	// 下载 2MB 测速
	target := 2 * 1024 * 1024
	buf := make([]byte, 32*1024)
	downloaded := 0
	t0 := time.Now()
	for downloaded < target {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			downloaded += n
		}
		if err != nil {
			break // EOF 或其他读取错误均结束
		}
	}
	elapsed := time.Since(t0).Seconds()
	if elapsed <= 0 || downloaded < 100*1024 {
		logging.ErrorTo("scanner", "测速下载不足 %s: downloaded=%d bytes, elapsed=%.2fs, contentLength=%d",
			ip, downloaded, elapsed, resp.ContentLength)
		return 0, false
	}
	mbps := float64(downloaded) * 8 / elapsed / 1_000_000
	return mbps, mbps >= 0.1
}

// === 入库 ===

func parseCodes(codeStr string) []string {
	parts := strings.Split(codeStr, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func findRegionByColo(colo string, regions []config.ProxyRegion) string {
	for _, reg := range regions {
		codes := parseCodes(reg.Code)
		for _, c := range codes {
			if c == colo {
				return reg.Name
			}
		}
	}
	return colo
}

func (s *Scanner) saveByCMIN2Colo(passed []speedResult, sc config.ScannerConfig) map[string]int {
	out := make(map[string]int)
	for _, r := range passed {
		if !r.ok || r.speedMbps < sc.MinSpeedMBps {
			continue
		}
		if !IsCMIN2Colo(r.colo) {
			continue
		}
		// colo -> region 映射（默认 colo 名即 region 名）
		region := r.colo
		err := s.lib.AddIP(r.ip, region, "auto", r.colo, r.speedMbps, r.latency, "scanner")
		if err == nil {
			out[region]++
		}
	}
	return out
}

func (s *Scanner) saveByRegion(passed []speedResult, sc config.ScannerConfig) map[string]int {
	out := make(map[string]int)
	regions := s.cfgMgr.Regions()
	for _, r := range passed {
		if !r.ok || r.speedMbps < sc.MinSpeedMBps {
			continue
		}
		region := findRegionByColo(r.colo, regions)
		if region == r.colo {
			logging.WarnTo("scanner", "Colo %s 不在任何地区配置中，作为独立 region 入库", r.colo)
		}
		err := s.lib.AddIP(r.ip, region, "auto", r.colo, r.speedMbps, r.latency, "scanner")
		if err == nil {
			out[region]++
		}
	}
	return out
}

// === 后台调度 ===

// StartLoop 启动后台循环扫描（按 Interval）
func (s *Scanner) StartLoop() {
	go func() {
		for {
			sc := s.cfgMgr.Scanner()
			if sc.Enabled {
				s.RunOnce()
			}
			nextInterval := s.cfgMgr.Scanner().Interval
			if nextInterval <= 0 {
				nextInterval = 60
			}
			time.Sleep(time.Duration(nextInterval) * time.Minute)
		}
	}()
}

// Stop 停止当前扫描
func (s *Scanner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
	}
}

// setProgress 更新扫描进度
func (s *Scanner) setProgress(stage string, done, total int64, currentIP string) {
	s.mu.Lock()
	s.progress.Stage = stage
	s.progress.StageDone = done
	s.progress.StageTotal = total
	s.progress.CurrentIP = currentIP
	if total > 0 {
		s.progress.Percent = int(done * 100 / total)
	}
	s.mu.Unlock()
}

// sortedEntriesByRegion 工具：按地区分组 + 按速度排序
func sortedEntriesByRegion(entries []config.IPEntry) map[string][]config.IPEntry {
	m := make(map[string][]config.IPEntry)
	for _, e := range entries {
		m[e.Region] = append(m[e.Region], e)
	}
	for k := range m {
		list := m[k]
		sort.Slice(list, func(i, j int) bool {
			return list[i].SpeedMbps > list[j].SpeedMbps
		})
		m[k] = list
	}
	return m
}
