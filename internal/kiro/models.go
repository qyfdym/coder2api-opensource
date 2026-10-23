package kiro

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Model Kiro 模型目录里的一项。
type Model struct {
	ID             string  `json:"id"`
	Name           string  `json:"name,omitempty"`
	Description    string  `json:"description,omitempty"`
	RateMultiplier float64 `json:"rate_multiplier,omitempty"` // 积分倍率（1 = 标准）
	MaxInput       int     `json:"max_input_tokens,omitempty"`
	MaxOutput      int     `json:"max_output_tokens,omitempty"`
	Vision         bool    `json:"vision,omitempty"`
	// Efforts / DefaultEffort 模型原生的思考档位（目录里 additionalModelRequestFieldsSchema 的
	// reasoning.effort，见 effort.go）；空 = 没有原生档位，思考靠提示词开关。
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
}

// fallbackModels 内置目录（动态目录拉不到时使用；与 Kiro-Go 的兜底列表一致）。
var fallbackModels = []Model{
	{ID: "claude-sonnet-4.6", Name: "Claude Sonnet 4.6", Vision: true, MaxInput: 1_000_000},
	{ID: "claude-opus-4.6", Name: "Claude Opus 4.6", Vision: true, MaxInput: 1_000_000},
	{ID: "claude-opus-4.7", Name: "Claude Opus 4.7", Vision: true, MaxInput: 1_000_000},
	{ID: "claude-sonnet-4.5", Name: "Claude Sonnet 4.5", Vision: true, MaxInput: 200_000},
	{ID: "claude-sonnet-4", Name: "Claude Sonnet 4", Vision: true, MaxInput: 200_000},
	{ID: "claude-haiku-4.5", Name: "Claude Haiku 4.5", Vision: true, MaxInput: 200_000},
	{ID: "claude-opus-4.5", Name: "Claude Opus 4.5", Vision: true, MaxInput: 200_000},
}

// modelCache 动态目录缓存：按账号分别缓存，对外是所有账号目录的并集。
//
// 上游按账号的套餐 / 授权返回目录（真机：学生版 Builder ID 号没有 Claude），所以不能用任意一个
// 账号的目录代表全池：池里同时有学生号和 Pro 号时，并集才是网关实际能服务的模型。
// 个别号不能用的模型由上游的模型错误（INVALID_MODEL_ID）+ 模型级冷却兜住，换号重试。
type modelCache struct {
	mu     sync.Mutex
	byUID  map[string]catalogEntry
	merged []Model // byUID 的并集（按 ID 排序）；空 = 还没拉过
}

type catalogEntry struct {
	models []Model
	at     time.Time
}

// maxCatalogAge 账号目录超过这个时间没刷新就不再并入全池目录（账号被删后残留的目录自然过期）。
const maxCatalogAge = 24 * time.Hour

// FallbackModels 内置目录副本。
func FallbackModels() []Model { return append([]Model(nil), fallbackModels...) }

// CachedModels 当前可用的目录（只读缓存，不打网络；没拉过时返回内置目录）。
func (c *Client) CachedModels() []Model {
	c.models.mu.Lock()
	defer c.models.mu.Unlock()
	if len(c.models.merged) > 0 {
		return append([]Model(nil), c.models.merged...)
	}
	return FallbackModels()
}

// HasCatalog 该账号是否拉过目录（不论是否过期）。
func (c *Client) HasCatalog(uid string) bool {
	c.models.mu.Lock()
	defer c.models.mu.Unlock()
	_, ok := c.models.byUID[uid]
	return ok
}

// CatalogFresh 该账号的目录是否还在缓存有效期内。
func (c *Client) CatalogFresh(uid string) bool {
	c.models.mu.Lock()
	defer c.models.mu.Unlock()
	e, ok := c.models.byUID[uid]
	return ok && time.Since(e.at) < c.opts.ModelCacheTTL
}

// Models 返回目录：该账号的目录缓存新鲜直接用；否则用 cred 拉一次动态目录（cred=nil 不打网络）。
// 拉取失败时退回旧缓存 / 内置目录，绝不返回空列表。
func (c *Client) Models(ctx context.Context, cred *Cred) []Model {
	if cred == nil || c.CatalogFresh(cred.UID) {
		return c.CachedModels()
	}
	if _, err := c.RefreshModels(ctx, *cred); err != nil {
		c.logf("models: %v (using cached / built-in catalog)", err)
	}
	return c.CachedModels()
}

// RefreshModels 用一个账号拉动态目录，写入该账号的缓存并重算全池目录。返回的是该账号自己的目录。
func (c *Client) RefreshModels(ctx context.Context, cred Cred) ([]Model, *Error) {
	list, err := c.ListModels(ctx, cred)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, &Error{Kind: KindServer, Msg: "ListAvailableModels returned no models"}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	c.models.mu.Lock()
	if c.models.byUID == nil {
		c.models.byUID = map[string]catalogEntry{}
	}
	c.models.byUID[cred.UID] = catalogEntry{models: append([]Model(nil), list...), at: time.Now()}
	c.models.rebuildLocked()
	c.models.mu.Unlock()
	return list, nil
}

// rebuildLocked 重算并集：丢弃过期账号目录；同一模型在多个账号里都有时，优先取带原生思考档位的那份。
func (m *modelCache) rebuildLocked() {
	byID := map[string]Model{}
	uids := make([]string, 0, len(m.byUID))
	for uid := range m.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids) // 同名模型取哪份要稳定，不能随 map 遍历顺序变
	for _, uid := range uids {
		e := m.byUID[uid]
		if time.Since(e.at) > maxCatalogAge {
			delete(m.byUID, uid)
			continue
		}
		for _, mm := range e.models {
			if old, ok := byID[mm.ID]; !ok || (len(old.Efforts) == 0 && len(mm.Efforts) > 0) {
				byID[mm.ID] = mm
			}
		}
	}
	m.merged = m.merged[:0:0]
	for _, mm := range byID {
		m.merged = append(m.merged, mm)
	}
	sort.Slice(m.merged, func(i, j int) bool { return m.merged[i].ID < m.merged[j].ID })
}

// normModelID 比较用的模型名：小写、版本号统一成点号（claude-sonnet-4-5 ≡ claude-sonnet-4.5）。
func normModelID(id string) string {
	m, _ := ParseModel(id, "")
	return strings.ToLower(strings.TrimSpace(m))
}

// LookupModel 在目录里找模型（大小写 / 版本号写法 / -thinking 后缀都容忍）。
func (c *Client) LookupModel(id string) (Model, bool) {
	base, _ := ParseModel(id, c.opts.ThinkingSuffix)
	want := normModelID(base)
	for _, m := range c.CachedModels() {
		if normModelID(m.ID) == want {
			return m, true
		}
	}
	return Model{}, false
}
