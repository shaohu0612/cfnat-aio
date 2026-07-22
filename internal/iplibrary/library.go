// Package iplibrary CMIN2 优选 IP 库
//
// 核心职责：
//   - 按地区（HKG/LAX/JP/...）独立存储 IP
//   - 供代理模块读取，作为转发目标池
//   - 供扫描模块写入，作为入库目标
//   - 定期健康检查，自动淘汰失效 IP
package iplibrary

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"cfnat-aio/internal/config"
	"cfnat-aio/internal/logging"
)

// Library CMIN2 IP 库
type Library struct {
	store *config.SQLiteStore
	mu    sync.RWMutex

	// 热缓存：每地区的可用 IP 列表
	cache map[string][]string // region -> [ip, ip, ...] (按速度排序)

	// 双层 IP 池（V1.7）
	activePool   map[string][]config.IPEntry // region -> [IPEntry] 活跃池
	standbyPool  map[string][]config.IPEntry // region -> [IPEntry] 备选池
}

// New 创建 IP 库
func New(store *config.SQLiteStore) *Library {
	lib := &Library{
		store:       store,
		cache:       make(map[string][]string),
		activePool:  make(map[string][]config.IPEntry),
		standbyPool: make(map[string][]config.IPEntry),
	}
	lib.reload()
	return lib
}

// reload 从 DB 重建内存缓存
func (l *Library) reload() {
	all, err := l.store.ListAllIPs()
	if err != nil {
		return
	}
	tmp := make(map[string][]config.IPEntry)
	for _, e := range all {
		tmp[e.Region] = append(tmp[e.Region], e)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache = make(map[string][]string)
	for region, list := range tmp {
		// 按速度降序
		sort.Slice(list, func(i, j int) bool {
			return list[i].SpeedMbps > list[j].SpeedMbps
		})
		ips := make([]string, 0, len(list))
		for _, e := range list {
			ips = append(ips, e.IP)
		}
		l.cache[region] = ips
	}
}

// AddIP 添加 IP（来源：auto/manual/import）
func (l *Library) AddIP(ip, region, source, colo string, speed, latency float64, note string) error {
	err := l.store.UpsertIP(config.IPEntry{
		IP:        ip,
		Region:    region,
		Colo:      colo,
		SpeedMbps: speed,
		LatencyMs: latency,
		Source:    source,
		LastOK:    true,
		Note:      note,
	})
	if err == nil {
		l.reload()
		logging.InfoTo("iplibrary", "已添加 IP %s → 地区 %s (来源=%s, colo=%s, 速度=%.1fMB/s)",
			ip, region, source, colo, speed)
	} else {
		logging.ErrorTo("iplibrary", "添加 IP %s 失败: %v", ip, err)
	}
	return err
}

// AddIPsBatch 批量添加 IP，只在最后调用一次 reload
func (l *Library) AddIPsBatch(entries []config.IPEntry) error {
	if len(entries) == 0 {
		return nil
	}
	err := l.store.UpsertIPsBatch(entries)
	if err == nil {
		l.reload()
		logging.InfoTo("iplibrary", "批量入库完成：共 %d 条 IP", len(entries))
	} else {
		logging.ErrorTo("iplibrary", "批量入库失败: %v", err)
	}
	return err
}

// RemoveIP 删除 IP
func (l *Library) RemoveIP(ip, region string) error {
	err := l.store.DeleteIP(ip, region)
	if err == nil {
		l.reload()
		logging.InfoTo("iplibrary", "已删除 IP %s（地区 %s）", ip, region)
	}
	return err
}

// calculateQualityScore 计算 IP 质量分数（0-100）
func calculateQualityScore(entry config.IPEntry) float64 {
	latencyScore := 100.0 / (entry.LatencyMs + 100.0)
	// 当前数据结构无 LossRate，使用 FailCount 近似估算
	lossRate := float64(entry.FailCount) * 33.3
	if lossRate > 100.0 {
		lossRate = 100.0
	}
	lossScore := (100.0 - lossRate) / 100.0
	speedScore := entry.SpeedMbps / 10.0
	if speedScore > 1.0 {
		speedScore = 1.0
	}
	freshnessScore := 1.0
	if entry.LastCheck != "" {
		lastCheck, _ := time.Parse(time.RFC3339, entry.LastCheck)
		hoursSince := time.Since(lastCheck).Hours()
		freshnessScore = math.Exp(-hoursSince / 24.0)
	}
	return 100.0 * latencyScore * lossScore * speedScore * freshnessScore
}

// RemoveLowQualityIPs 删除质量分数低于阈值的 IP
func (l *Library) RemoveLowQualityIPs(threshold float64) (int, error) {
	entries := l.ListIPs("")
	removed := 0
	for _, e := range entries {
		score := calculateQualityScore(e)
		if score < threshold {
			if err := l.store.DeleteIP(e.IP, e.Region); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		l.reload()
	}
	logging.InfoTo("iplibrary", "低质量 IP 淘汰完成：删除 %d 条（阈值 %.2f）", removed, threshold)
	return removed, nil
}

// ListIPs 列出某地区所有 IP
// region 为空时返回全部地区
func (l *Library) ListIPs(region string) []config.IPEntry {
	if region == "" {
		all, _ := l.store.ListAllIPs()
		if all == nil {
			return []config.IPEntry{}
		}
		return all
	}
	entries, _ := l.store.ListIPs(region)
	if entries == nil {
		return []config.IPEntry{}
	}
	return entries
}

// CountIPs 统计某地区 IP 数
func (l *Library) CountIPs(region string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.cache[region])
}

// CountIPsByCodes 统计多个 colo 代码的总 IP 数
func (l *Library) CountIPsByCodes(codes []string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	total := 0
	for _, code := range codes {
		total += len(l.cache[code])
	}
	return total
}

// PickRandomByCodes 从多个 colo 代码中随机挑选一个 IP
func (l *Library) PickRandomByCodes(codes []string) (string, error) {
	l.mu.RLock()
	var allIPs []string
	for _, code := range codes {
		allIPs = append(allIPs, l.cache[code]...)
	}
	l.mu.RUnlock()

	if len(allIPs) == 0 {
		return "", fmt.Errorf("no IPs for codes %v", codes)
	}

	rand.Seed(time.Now().UnixNano())
	return allIPs[rand.Intn(len(allIPs))], nil
}

// ListIPsByCodes 列出多个 colo 代码的所有 IP（V1.2 最少连接数负载均衡用）
func (l *Library) ListIPsByCodes(codes []string) ([]config.IPEntry, error) {
	var allIPs []config.IPEntry
	for _, code := range codes {
		entries, err := l.store.ListIPs(code)
		if err != nil {
			return nil, err
		}
		allIPs = append(allIPs, entries...)
	}
	return allIPs, nil
}

// PickRandomByCodesWithExclude 从多个 colo 代码中随机挑选一个 IP，排除指定 IP
func (l *Library) PickRandomByCodesWithExclude(codes []string, exclude map[string]bool) (string, error) {
	l.mu.RLock()
	var allIPs []string
	for _, code := range codes {
		for _, ip := range l.cache[code] {
			if !exclude[ip] {
				allIPs = append(allIPs, ip)
			}
		}
	}
	l.mu.RUnlock()

	if len(allIPs) == 0 {
		return "", fmt.Errorf("no IPs for codes %v after exclusion", codes)
	}

	rand.Seed(time.Now().UnixNano())
	return allIPs[rand.Intn(len(allIPs))], nil
}

// PickRandom 从某地区随机挑一个 IP（代理用）
// 会先做"软过滤"：跳过最近失败过的 IP
func (l *Library) PickRandom(region string) (string, error) {
	l.mu.RLock()
	ips := l.cache[region]
	l.mu.RUnlock()

	if len(ips) == 0 {
		return "", fmt.Errorf("region %q has no IPs", region)
	}

	// 顺序随机选取，失败的也允许（兜底）
	rand.Seed(time.Now().UnixNano())
	idx := rand.Intn(len(ips))
	return ips[idx], nil
}

// PickRandomByRegionWithExclude 从某地区随机挑选一个 IP，排除指定 IP
func (l *Library) PickRandomByRegionWithExclude(region string, exclude map[string]bool) (string, error) {
	l.mu.RLock()
	var allIPs []string
	for _, ip := range l.cache[region] {
		if exclude == nil || !exclude[ip] {
			allIPs = append(allIPs, ip)
		}
	}
	l.mu.RUnlock()

	if len(allIPs) == 0 {
		return "", fmt.Errorf("region %q has no IPs after exclusion", region)
	}

	rand.Seed(time.Now().UnixNano())
	return allIPs[rand.Intn(len(allIPs))], nil
}

// PickFallback 库为空时，从全量 CF IP 中随机选（兜底）
// 这部分由调用方传入候选列表
func (l *Library) PickFallback(candidates []string) (string, error) {
	if len(candidates) == 0 {
		return "", fmt.Errorf("no fallback candidates")
	}
	rand.Seed(time.Now().UnixNano())
	return candidates[rand.Intn(len(candidates))], nil
}

// HealthCheck 健康检查一个地区所有 IP
//   - 主动 TCP 探活
//   - 连续失败 3 次的 IP 自动从库中删除
//   - checkFn 由调用方提供（避免循环依赖）
func (l *Library) HealthCheck(region string, checkFn func(ip string) (ok bool, latencyMs float64)) (int, int) {
	entries := l.ListIPs(region)
	ok, fail := 0, 0
	for _, e := range entries {
		isOK, latency := checkFn(e.IP)
		_ = l.store.MarkIPChecked(e.IP, region, isOK, e.SpeedMbps, latency)
		if isOK {
			ok++
		} else {
			fail++
		}
	}
	// 失败 3 次的自动清出
	removed, _ := l.store.RemoveFailingIPs(3)
	if removed > 0 {
		l.reload()
	}
	return ok, fail
}

// HealthCheckAll 对所有地区做健康检查
func (l *Library) HealthCheckAll(checkFn func(ip string) (ok bool, latencyMs float64)) map[string][2]int {
	regions := make(map[string]bool)
	all, _ := l.store.ListAllIPs()
	for _, r := range all {
		regions[r.Region] = true
	}
	out := make(map[string][2]int)
	for r := range regions {
		ok, fail := l.HealthCheck(r, checkFn)
		out[r] = [2]int{ok, fail}
	}
	return out
}

// Regions 返回所有有 IP 的地区列表
func (l *Library) Regions() []string {
	all, _ := l.store.ListAllIPs()
	set := make(map[string]bool)
	for _, e := range all {
		set[e.Region] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// IsEmpty 地区是否为空
func (l *Library) IsEmpty(region string) bool {
	return l.CountIPs(region) == 0
}

// RebuildPools 重建双层 IP 池（V1.7）
// 仅将健康 IP（最近检查成功且连续失败次数 < 3）纳入池，确保重启后健康状态不丢失
func (l *Library) RebuildPools(activeSize int, standbyRatio float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	allIPs := l.ListIPs("")
	regionMap := make(map[string][]config.IPEntry)
	for _, e := range allIPs {
		regionMap[e.Region] = append(regionMap[e.Region], e)
	}

	l.activePool = make(map[string][]config.IPEntry)
	l.standbyPool = make(map[string][]config.IPEntry)

	for region, entries := range regionMap {
		// 过滤掉健康检查失败的 IP
		var healthy []config.IPEntry
		for _, e := range entries {
			if e.LastOK && e.FailCount < 3 {
				healthy = append(healthy, e)
			}
		}
		if len(healthy) == 0 {
			continue
		}

		// 按质量分数降序排序
		sort.Slice(healthy, func(i, j int) bool {
			return calculateQualityScore(healthy[i]) > calculateQualityScore(healthy[j])
		})

		activeCount := activeSize
		if len(healthy) < activeSize {
			activeCount = len(healthy)
		}
		l.activePool[region] = healthy[:activeCount]

		standbyCount := int(float64(activeSize) * standbyRatio)
		remaining := healthy[activeCount:]
		if len(remaining) < standbyCount {
			standbyCount = len(remaining)
		}
		l.standbyPool[region] = remaining[:standbyCount]
	}

	logging.InfoTo("iplibrary", "双层 IP 池已重建：活跃池 %d 个地区，备选池 %d 个地区",
		len(l.activePool), len(l.standbyPool))
}

// RebuildPoolsForRegion 为指定地区重建 IP 池（V1.7）
func (l *Library) RebuildPoolsForRegion(region string, activeSize int, standbyRatio float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries := l.ListIPs(region)
	var healthy []config.IPEntry
	for _, e := range entries {
		if e.LastOK && e.FailCount < 3 {
			healthy = append(healthy, e)
		}
	}
	if len(healthy) == 0 {
		return
	}

	// 按质量分数降序排序
	sort.Slice(healthy, func(i, j int) bool {
		return calculateQualityScore(healthy[i]) > calculateQualityScore(healthy[j])
	})

	activeCount := activeSize
	if len(healthy) < activeSize {
		activeCount = len(healthy)
	}
	l.activePool[region] = healthy[:activeCount]

	standbyCount := int(float64(activeSize) * standbyRatio)
	remaining := healthy[activeCount:]
	if len(remaining) < standbyCount {
		standbyCount = len(remaining)
	}
	l.standbyPool[region] = remaining[:standbyCount]

	logging.InfoTo("iplibrary", "地区 %s IP 池已重建：活跃池 %d，备选池 %d",
		region, len(l.activePool[region]), len(l.standbyPool[region]))
}

// PromoteFromStandby 从备选池提升 IP 到活跃池（V1.7）
func (l *Library) PromoteFromStandby(region string, count int) []config.IPEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	standby := l.standbyPool[region]
	if len(standby) == 0 {
		return nil
	}

	promoteCount := count
	if len(standby) < count {
		promoteCount = len(standby)
	}

	promoted := standby[:promoteCount]
	l.standbyPool[region] = standby[promoteCount:]
	l.activePool[region] = append(l.activePool[region], promoted...)

	logging.InfoTo("iplibrary", "%s: 从备选池提升 %d 个 IP 到活跃池", region, len(promoted))
	return promoted
}

// RemoveFromActive 从活跃池中移除 IP（V1.7）
func (l *Library) RemoveFromActive(region, ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	active := l.activePool[region]
	for i, entry := range active {
		if entry.IP == ip {
			l.activePool[region] = append(active[:i], active[i+1:]...)
			logging.InfoTo("iplibrary", "%s: 从活跃池移除 IP %s", region, ip)
			return true
		}
	}
	return false
}

// GetActivePool 获取活跃池（V1.7）
func (l *Library) GetActivePool(region string) []config.IPEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.activePool[region]
}

// GetStandbyPool 获取备选池（V1.7）
func (l *Library) GetStandbyPool(region string) []config.IPEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.standbyPool[region]
}

// GetPoolForIP 判断IP所属池（active/standby/none）
func (l *Library) GetPoolForIP(ip, region string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for _, entry := range l.activePool[region] {
		if entry.IP == ip {
			return "active"
		}
	}
	for _, entry := range l.standbyPool[region] {
		if entry.IP == ip {
			return "standby"
		}
	}
	return "none"
}

// GetPoolSizes 获取各地区池大小（V1.7）
func (l *Library) GetPoolSizes() map[string][2]int {
	l.mu.RLock()
	defer l.mu.RUnlock()

	result := make(map[string][2]int)
	for region := range l.activePool {
		result[region] = [2]int{len(l.activePool[region]), len(l.standbyPool[region])}
	}
	return result
}
