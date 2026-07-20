package proxy

import (
	"sync"
	"time"
)

type IPMetrics struct {
	ewmaDelay   float64
	ewmaLoss    float64
	sampleCount int
	lastActive  time.Time
	createdAt   time.Time
	mu          sync.RWMutex
}

func NewIPMetrics() *IPMetrics {
	return &IPMetrics{
		createdAt:  time.Now(),
		lastActive: time.Now(),
	}
}

func (m *IPMetrics) UpdateDelay(delayMs float64, alpha float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sampleCount++
	m.lastActive = time.Now()
	if m.ewmaDelay == 0 {
		m.ewmaDelay = delayMs
	} else {
		m.ewmaDelay = m.ewmaDelay*(1-alpha) + delayMs*alpha
	}
}

func (m *IPMetrics) UpdateLoss(isLoss bool, alpha float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sampleCount++
	m.lastActive = time.Now()
	lossVal := 0.0
	if isLoss {
		lossVal = 1.0
	}
	if m.ewmaLoss == 0 {
		m.ewmaLoss = lossVal
	} else {
		m.ewmaLoss = m.ewmaLoss*(1-alpha) + lossVal*alpha
	}
}

func (m *IPMetrics) GetDelay() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ewmaDelay
}

func (m *IPMetrics) GetLossRate() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ewmaLoss * 100
}

func (m *IPMetrics) GetSampleCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sampleCount
}

func (m *IPMetrics) GetLastActive() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastActive
}

func (m *IPMetrics) IsStale(maxAge time.Duration) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return time.Since(m.lastActive) > maxAge
}

type MetricsManager struct {
	metrics sync.Map
	mu      sync.Mutex
}

func NewMetricsManager() *MetricsManager {
	return &MetricsManager{
		metrics: sync.Map{},
	}
}

func (mm *MetricsManager) Get(ip string) *IPMetrics {
	if val, ok := mm.metrics.Load(ip); ok {
		return val.(*IPMetrics)
	}
	mm.metrics.Store(ip, NewIPMetrics())
	if val, ok := mm.metrics.Load(ip); ok {
		return val.(*IPMetrics)
	}
	return nil
}

func (mm *MetricsManager) Remove(ip string) {
	mm.metrics.Delete(ip)
}

func (mm *MetricsManager) GetAll() map[string]*IPMetrics {
	result := make(map[string]*IPMetrics)
	mm.metrics.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(*IPMetrics)
		return true
	})
	return result
}

func (mm *MetricsManager) Cleanup(maxAge time.Duration) int {
	count := 0
	mm.metrics.Range(func(key, value interface{}) bool {
		ip := key.(string)
		m := value.(*IPMetrics)
		if m.IsStale(maxAge) {
			mm.metrics.Delete(ip)
			count++
		}
		return true
	})
	return count
}
