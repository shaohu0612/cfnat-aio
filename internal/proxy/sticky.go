package proxy

import (
	"sync"
	"time"
)

type StickySlot struct {
	backendIP  string
	lastAccess time.Time
}

type StickyManager struct {
	mu       sync.Mutex
	slots    map[string]*StickySlot
	maxSlots int
	ttl      time.Duration
}

func NewStickyManager(maxSlots int, ttl time.Duration) *StickyManager {
	return &StickyManager{
		slots:    make(map[string]*StickySlot),
		maxSlots: maxSlots,
		ttl:      ttl,
	}
}

func (sm *StickyManager) Get(clientIP, region string) (string, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := clientIP + ":" + region
	slot, ok := sm.slots[key]
	if !ok {
		return "", false
	}

	if time.Since(slot.lastAccess) > sm.ttl {
		delete(sm.slots, key)
		return "", false
	}

	slot.lastAccess = time.Now()
	return slot.backendIP, true
}

func (sm *StickyManager) Set(clientIP, region, backendIP string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := clientIP + ":" + region
	
	if len(sm.slots) >= sm.maxSlots {
		oldestKey := ""
		var oldestTime time.Time
		for k, s := range sm.slots {
			if oldestKey == "" || s.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = s.lastAccess
			}
		}
		if oldestKey != "" {
			delete(sm.slots, oldestKey)
		}
	}

	sm.slots[key] = &StickySlot{
		backendIP:  backendIP,
		lastAccess: time.Now(),
	}
}

func (sm *StickyManager) Remove(clientIP, region string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := clientIP + ":" + region
	delete(sm.slots, key)
}

func (sm *StickyManager) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.slots = make(map[string]*StickySlot)
}

func (sm *StickyManager) Count() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return len(sm.slots)
}

func (sm *StickyManager) Cleanup() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	count := 0
	now := time.Now()
	for key, slot := range sm.slots {
		if now.Sub(slot.lastAccess) > sm.ttl {
			delete(sm.slots, key)
			count++
		}
	}
	return count
}
