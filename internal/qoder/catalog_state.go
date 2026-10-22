package qoder

import (
	"sync"
	"time"
)

// catalogState 模型目录的进程内状态（内存缓存、动态拉取的去重锁）。
//
// 本文件刻意只引用标准库类型：目录的具体数据结构（*realmCatalog，见 catalog.go）
// 以 any 存放，由 catalog.go 断言取用。这样 client.go 与并行开发的其它文件只依赖
// 这里的两个符号（catalogState / newCatalogState），目录实现怎么改都牵连不到它们。
type catalogState struct {
	mu sync.Mutex
	// realms 区域 → 缓存项。
	realms map[string]*catalogCacheEntry
	// fetchMu 区域 → 动态拉取互斥（同一区域同一时刻只打一次官方接口，其余等待后读缓存）。
	fetchMu map[string]*sync.Mutex
}

// catalogCacheEntry 单个区域的目录缓存。
type catalogCacheEntry struct {
	value  any       // *realmCatalog（catalog.go）
	at     time.Time // 写入时刻（内存缓存 5 分钟有效）
	source string    // dynamic / disk / snapshot
	// dynAt 最近一次尝试动态接口的时刻（无论成败）：动态失败后 5 分钟内带凭证的
	// 调用不再重复打官方接口，避免每次 /v1/models 都卡一个超时。
	dynAt time.Time
}

func newCatalogState() *catalogState {
	return &catalogState{
		realms:  map[string]*catalogCacheEntry{},
		fetchMu: map[string]*sync.Mutex{},
	}
}

// lockFetch 取某区域的动态拉取锁（惰性创建）。
func (s *catalogState) lockFetch(realm string) *sync.Mutex {
	s.mu.Lock()
	m := s.fetchMu[realm]
	if m == nil {
		m = &sync.Mutex{}
		s.fetchMu[realm] = m
	}
	s.mu.Unlock()
	return m
}

func (s *catalogState) load(realm string) *catalogCacheEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.realms[realm]
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

func (s *catalogState) store(realm string, e *catalogCacheEntry) {
	s.mu.Lock()
	s.realms[realm] = e
	s.mu.Unlock()
}
