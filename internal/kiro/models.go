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

// modelCache 动态目录缓存（任一账号拉到的目录全池共用：Kiro 的模型目录按套餐差异很小，
// 个别号不能用的模型由上游的模型错误 + 模型级冷却兜住）。
type modelCache struct {
	mu     sync.Mutex
	models []Model
	at     time.Time
}

// FallbackModels 内置目录副本。
func FallbackModels() []Model { return append([]Model(nil), fallbackModels...) }

// CachedModels 当前可用的目录（只读缓存，不打网络；没拉过时返回内置目录）。
func (c *Client) CachedModels() []Model {
	c.models.mu.Lock()
	defer c.models.mu.Unlock()
	if len(c.models.models) > 0 {
		return append([]Model(nil), c.models.models...)
	}
	return FallbackModels()
}

// Models 返回目录：缓存新鲜直接用；否则用 cred 拉一次动态目录（cred=nil 不打网络）。
// 拉取失败时退回旧缓存 / 内置目录，绝不返回空列表。
func (c *Client) Models(ctx context.Context, cred *Cred) []Model {
	c.models.mu.Lock()
	fresh := len(c.models.models) > 0 && time.Since(c.models.at) < c.opts.ModelCacheTTL
	c.models.mu.Unlock()
	if fresh || cred == nil {
		return c.CachedModels()
	}
	if _, err := c.RefreshModels(ctx, *cred); err != nil {
		c.logf("models: %v (using cached / built-in catalog)", err)
	}
	return c.CachedModels()
}

// RefreshModels 用一个账号拉动态目录并写缓存。
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
	c.models.models = append([]Model(nil), list...)
	c.models.at = time.Now()
	c.models.mu.Unlock()
	return list, nil
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
