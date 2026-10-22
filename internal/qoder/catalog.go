package qoder

// 模型目录：官方动态接口 → 磁盘缓存 → 内置快照，三级降级。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）：
// qoder_catalog.py（快照、别名、显示名索引、官方文案）与 qoder_proxy.py 的
// fetch_models / read_dynamic_models / merge_catalog / model_entry / is_chat_model。
// 有意未移植：读本机官方客户端缓存 catalog-v6（QMC 解密）、dynamic-text 文案文件探测
// （用原实现内嵌的文案快照）、双区自动选区（网关账号池负责）。
//
// 三源口径差异是**数据源不同**而不是 bug：动态接口是官方「此刻」，磁盘缓存是上次成功
// 拉取的时刻，内置快照是导出时刻；降级到哪一级，价格倍率 / 峰谷价 / 默认档位就换一套。

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// chatPath 推理 SSE 端点（FetchKeys / AgentId / Encode 三个查询参数与官方一致）。
	chatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation" +
		"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	// modelsPath 官方动态模型清单（COSY 签名，需要账号）。
	modelsPath = "/algo/api/v2/model/list?Encode=1"

	catalogMemTTL  = 5 * time.Minute    // 内存缓存（原实现 fetch_models 的 300s）
	catalogDiskTTL = 7 * 24 * time.Hour // 磁盘缓存（原实现 _MODELS_DISK_CACHE_TTL，issue #58）

	// officialDisabledReason 未开通套餐时官方桌面版所示的禁用文案（原文照搬）。
	officialDisabledReason = "需要升级或购买千问官方套餐开放"

	sourceDynamic  = "dynamic"
	sourceDisk     = "disk"
	sourceSnapshot = "snapshot"
)

//go:embed assets/qoder_catalog_cn.json
var snapshotCNJSON []byte

//go:embed assets/qoder_catalog_intl.json
var snapshotIntlJSON []byte

// Model 一个 Qoder 模型（/v1/models 展示 + 路由用）。
type Model struct {
	ID                string   // 官方模型名，如 "Qwen3.8-Max"；网关对外以 "qoder:"+ID 暴露
	Key               string   // 上游 model key（x-model-key），如 "qmodel_38max"
	Realm             string   // cn / intl
	Name              string   // 本地化名称（官方 zh.label；无则同 ID）
	Description       string   // 官方介绍文案
	ContextLength     int64    // 默认输入上限（max_input_tokens）
	MaxOutputTokens   int64    // 0 = 官方未提供（不编造）
	Efforts           []string // 合法思考档位（low < medium < high < xhigh < max 顺序）
	DefaultEffort     string   // 官方 is_default 档位（无则空）
	Vision            bool
	ToolCalls         bool
	Reasoning         bool
	PriceFactor       float64  // 当前计费倍率（官方 price_factor）
	PriceFactorValley *float64 // 低谷倍率 = 峰值 × 折扣；nil = 无峰谷价（官方未给 promotion）
	Enabled           bool
	DisabledReason    string
	Aliases           []string // 全部可接受写法（key / "key (Name)" / 人类别名）
}

// catEntry 目录条目：上游 key + 官方原始元数据（保序，全字段原样）。
type catEntry struct {
	key  string
	meta *jval
}

// realmCatalog 一个区域合并后的目录（catalogCacheEntry.value 的实际类型）。
type realmCatalog struct {
	realm   string
	entries []catEntry
	models  []Model
	byKey   map[string]int
}

// ---------------------------------------------------------------------------
// 内置快照
// ---------------------------------------------------------------------------

var (
	snapshotOnce sync.Once
	snapshotCN   []catEntry
	snapshotIntl []catEntry
)

func loadSnapshots() {
	snapshotOnce.Do(func() {
		snapshotCN = parseSnapshot(snapshotCNJSON)
		snapshotIntl = parseSnapshot(snapshotIntlJSON)
	})
}

func parseSnapshot(raw []byte) []catEntry {
	v, err := parseJ(raw)
	if err != nil || !v.isArr() {
		panic("qoder: bad embedded catalog snapshot: " + fmt.Sprint(err))
	}
	out := make([]catEntry, 0, len(v.arr))
	for _, m := range v.arr {
		if !m.isObj() {
			continue
		}
		key := m.get("key").str()
		if key == "" {
			continue
		}
		out = append(out, catEntry{key: key, meta: m})
	}
	return out
}

// staticModels 该区域的内置快照（未知区域按国内版，与 models_for_realm 一致）。
func staticModels(realm string) []catEntry {
	loadSnapshots()
	if NormalizeRealm(realm) == RealmIntl {
		return snapshotIntl
	}
	return snapshotCN
}

// staticAll 原实现 STATIC_CN_MODELS + STATIC_INTL_MODELS（国内在前）。
func staticAll() []catEntry {
	loadSnapshots()
	out := make([]catEntry, 0, len(snapshotCN)+len(snapshotIntl))
	out = append(out, snapshotCN...)
	return append(out, snapshotIntl...)
}

// ---------------------------------------------------------------------------
// 别名与官方文案（qoder_catalog.MODEL_ALIASES / _EMBEDDED_MODEL_TEXT 原样）
// ---------------------------------------------------------------------------

type kvPair struct{ k, v string }

// modelAliases 客户端友好名 / 官方显示名 → 上游 key（顺序即原实现 dict 顺序，
// 决定 Model.Aliases 的排列）。
var modelAliases = []kvPair{
	{"auto", "auto"}, {"qoder-auto", "auto"},
	{"qwen3.8-max", "qmodel_38max"},
	{"qwen3.8-max-preview", "qmodel_38max"},
	{"qmodel_preview", "qmodel_38max"}, // 旧 key -> 新 key
	{"qwen3.8-flash", "qfmodel"},
	{"qwen3.7-max", "qmodel_latest"},
	{"qwen3.7-plus", "qmodel"},
	{"qwen3.7-flash", "q37fmodel"},
	{"qwen3.6-flash", "q36fmodel"},
	{"deepseek-v4-pro", "dmodel"},
	{"deepseek-v4-flash", "dfmodel"},
	{"deepseek-flash", "dfmodel"},
	{"glm-5.3", "gmodel"},
	{"glm-5.3-flash", "gfmodel"},
	{"glm-5.2", "gm51model"},
	{"kimi-k3", "kmodel_latest"},
	{"kimi-k2.8-preview", "kmodel"},
	{"kimi-k2.7", "kmodel"},
	{"kimi-k2.7-code", "kmodel"},
	{"minimax-m3", "mmodel"},
	{"minimax-m2.7", "mmodel"},
	{"minimax-m2.5", "mmodel"},
}

var modelAliasMap = func() map[string]string {
	m := make(map[string]string, len(modelAliases))
	for _, p := range modelAliases {
		m[p.k] = p.v
	}
	return m
}()

// officialDescriptions 官方桌面版模型介绍（原实现内嵌快照，issue #68 已去版本号）。
var officialDescriptions = map[string]string{
	"auto":          "智能选择最适合的模型，平衡性能与成本",
	"lite":          "基础推理能力，免费使用（高峰期可能响应较慢）",
	"cmodel":        "尝鲜体验全球顶级模型，擅长超长自主任务执行",
	"dmodel":        "复杂推理、代码生成与工程任务表现卓越",
	"gmodel":        "智谱开源旗舰模型，代码能力对标国际顶尖水平，兼具安全审查与漏洞推理能力",
	"kmodel":        "专为长上下文编程打造：精准遵循指令，可靠执行长链路任务",
	"mmodel":        "原生多模态感知、前沿编码能力，以及 1M 上下文深度，适合高强度工作流",
	"qmodel":        "推理能力、效率与多模态体验的全面跃迁",
	"dfmodel":       "推理速度快且成本低廉，综合能力均衡",
	"gfmodel":       "智谱全新原生多模态模型，深度理解图像与视频，自主完成研究分析、文档制作等复杂任务",
	"qfmodel":       "千问开源权重的多模态 MoE 模型，在能力、延迟与成本间取得出色平衡",
	"ultimate":      "专家级深度推理与思考能力，极致输出质量。",
	"efficient":     "标准推理能力，高性价比",
	"performance":   "高级推理能力，高质量输出",
	"qmodel_38max":  "Qwen 最新基座模型，拥有 2.4T 参数，在软件工程、办公与深度推理方面表现领先",
	"kmodel_latest": "Kimi 最强模型，拥有 2.8T 参数，专为软件工程、知识工作与深度推理打造",
	"qmodel_latest": "Agentic 能力的全面跃迁，可自主完成长程复杂任务",
}

// officialLabels 官方本地化显示名（zh.label，顺序即原实现 dict 顺序）。
var officialLabels = []kvPair{
	{"auto", "Auto"}, {"lite", "轻量"}, {"cmodel", "Cantus"},
	{"dmodel", "DeepSeek-V4-Pro"}, {"gmodel", "GLM-5.3"},
	{"kmodel", "Kimi-K2.8-Preview"}, {"mmodel", "MiniMax-M3"},
	{"qmodel", "Qwen3.7-Plus"}, {"dfmodel", "DeepSeek-V4-Flash"},
	{"gfmodel", "GLM-5.3-Flash"}, {"qfmodel", "Qwen3.8-Flash"},
	{"ultimate", "极致"}, {"efficient", "经济"}, {"performance", "性能"},
	{"qmodel_38max", "Qwen3.8-Max"}, {"kmodel_latest", "Kimi-K3"},
	{"qmodel_latest", "Qwen3.7-Max"},
}

func officialLabel(key string) string {
	for _, p := range officialLabels {
		if p.k == key {
			return p.v
		}
	}
	return ""
}

// officialLocalName 本地化名；与快照 display_name 相同时返回空（official_local_name）。
func officialLocalName(key string) string {
	lab := officialLabel(key)
	if lab == "" {
		return ""
	}
	for _, e := range staticAll() {
		if e.key == key {
			if lab == e.meta.get("display_name").str() {
				return ""
			}
			return lab
		}
	}
	return lab
}

// ---------------------------------------------------------------------------
// 非 chat 模型过滤（is_chat_model）
// ---------------------------------------------------------------------------

var (
	nonChatModels   = map[string]bool{"lite": true}
	nonChatPrefixes = []string{"codewise-", "completion-"}
	nonChatSuffixes = []string{"-image-alpha", "-image-alpha-edit", "-taco-completion"}
)

func isChatModel(mid string) bool {
	if mid == "" || nonChatModels[mid] {
		return false
	}
	for _, p := range nonChatPrefixes {
		if strings.HasPrefix(mid, p) {
			return false
		}
	}
	for _, s := range nonChatSuffixes {
		if strings.HasSuffix(mid, s) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 合并（merge_catalog）
// ---------------------------------------------------------------------------

// mergeCatalog 静态快照打底补元数据；**清单以 primary 为准**（官方此刻列什么就是什么，
// 不额外塞静态独有条目）；primary 为空时回退静态全量。null 值不覆盖静态值（动态接口
// 对部分字段返回 null，静默覆盖会抹掉快照里的真实值）。输出按官方 UI 顺序（静态顺序）。
func mergeCatalog(primary []catEntry, realm string) []catEntry {
	static := staticModels(realm)
	var order []string
	merged := map[string]*jval{}
	var mergedOrder []string
	for _, it := range static {
		order = append(order, it.key)
		if isChatModel(it.key) {
			if _, ok := merged[it.key]; !ok {
				mergedOrder = append(mergedOrder, it.key)
			}
			merged[it.key] = it.meta.shallow()
		}
	}
	var primaryKeys map[string]bool
	for _, p := range primary {
		if !isChatModel(p.key) {
			continue
		}
		if p.meta.truthy() {
			base := merged[p.key]
			if base == nil {
				base = &jval{k: jObj}
				mergedOrder = append(mergedOrder, p.key)
			}
			for i, k := range p.meta.keys {
				if !p.meta.vals[i].isNull() {
					base.set(k, p.meta.vals[i])
				}
			}
			merged[p.key] = base
		} else if _, ok := merged[p.key]; !ok {
			merged[p.key] = &jval{k: jObj}
			mergedOrder = append(mergedOrder, p.key)
		}
	}
	if len(primary) > 0 {
		primaryKeys = map[string]bool{}
		for _, p := range primary {
			if isChatModel(p.key) {
				primaryKeys[p.key] = true
			}
		}
	}
	wanted := func(mid string) bool {
		if !isChatModel(mid) {
			return false
		}
		if primaryKeys != nil && !primaryKeys[mid] {
			return false
		}
		return true
	}
	out := make([]catEntry, 0, len(merged))
	seen := map[string]bool{}
	for _, mid := range order {
		if m, ok := merged[mid]; ok && wanted(mid) && !seen[mid] {
			out = append(out, catEntry{key: mid, meta: m})
			seen[mid] = true
		}
	}
	for _, mid := range mergedOrder {
		if !seen[mid] && wanted(mid) {
			out = append(out, catEntry{key: mid, meta: merged[mid]})
			seen[mid] = true
		}
	}
	return out
}

func newRealmCatalog(realm string, entries []catEntry) *realmCatalog {
	rc := &realmCatalog{realm: realm, entries: entries, byKey: map[string]int{}}
	for i, e := range entries {
		rc.byKey[e.key] = i
		rc.models = append(rc.models, modelFromEntry(e.key, e.meta, realm))
	}
	return rc
}

// ---------------------------------------------------------------------------
// 条目 → Model（model_entry 的取值口径）
// ---------------------------------------------------------------------------

var effortOrder = []string{"low", "medium", "high", "xhigh", "max"}

func jInt64(v *jval) int64 {
	f, ok := v.num()
	if !ok {
		return 0
	}
	return int64(f)
}

func modelFromEntry(key string, meta *jval, realm string) Model {
	if meta == nil {
		meta = &jval{k: jObj}
	}
	display := meta.get("name").str()
	if display == "" {
		display = meta.get("display_name").str()
	}
	if display == "" {
		display = key
	}
	m := Model{ID: display, Key: key, Realm: NormalizeRealm(realm)}
	en := meta.get("enable")
	m.Enabled = !(en != nil && en.k == jBool && !en.b)
	if lab := officialLocalName(key); lab != "" {
		m.Name = lab
	} else {
		m.Name = display
	}
	m.Description = meta.get("description").str()
	if m.Description == "" {
		m.Description = officialDescriptions[key]
	}
	if display != key {
		m.Aliases = []string{key, key + " (" + display + ")"}
	} else {
		m.Aliases = []string{key}
	}
	for _, p := range modelAliases {
		if p.v == key && !containsStr(m.Aliases, p.k) {
			m.Aliases = append(m.Aliases, p.k)
		}
	}
	if !m.Enabled {
		m.DisabledReason = officialDisabledReason
	}
	m.Vision = (meta.get("is_vl").truthy() || meta.get("supportsImages").truthy()) &&
		!meta.get("disabledMultimodal").truthy()
	m.ToolCalls = true
	if st := meta.get("supportsToolCall"); st != nil {
		m.ToolCalls = st.truthy()
	}
	tc := meta.get("thinking_config")
	if !tc.isObj() {
		tc = &jval{k: jObj}
	}
	m.Reasoning = meta.get("is_reasoning").truthy() || meta.get("supportsReasoning").truthy() ||
		tc.get("enabled").isObj()
	maxIn := meta.get("maxInputTokens")
	if !maxIn.truthy() {
		maxIn = meta.get("max_input_tokens")
	}
	m.ContextLength = jInt64(maxIn)
	maxOut := meta.get("maxOutputTokens")
	if !maxOut.truthy() {
		maxOut = meta.get("max_output_tokens")
	}
	m.MaxOutputTokens = jInt64(maxOut)
	if en := tc.get("enabled"); en.isObj() {
		effMap := en.get("efforts")
		for _, e := range effortOrder {
			if effMap.has(e) {
				m.Efforts = append(m.Efforts, e)
			}
		}
		for _, e := range m.Efforts {
			if x := effMap.get(e); x.isObj() && x.get("is_default").truthy() {
				m.DefaultEffort = e
				break
			}
		}
	}
	// 兼容旧 catalog 形态（reasoning.supportedEfforts / defaultEffort）
	if old := meta.get("reasoning"); old.isObj() {
		if len(m.Efforts) == 0 {
			if se := old.get("supportedEfforts"); se.isArr() {
				for _, x := range se.arr {
					if s := x.str(); s != "" {
						m.Efforts = append(m.Efforts, s)
					}
				}
			}
		}
		if m.DefaultEffort == "" {
			m.DefaultEffort = old.get("defaultEffort").str()
		}
	}
	if pf, ok := meta.get("price_factor").num(); ok {
		m.PriceFactor = pf
	}
	m.PriceFactorValley = valleyPriceFactor(meta)
	return m
}

// valleyPriceFactor 低谷价 = 峰值（before_promotion_price_factor，缺省 original_price_factor）
// × promotion.discount_factor（0 < d ≤ 1），保留 4 位小数（model_entry 口径）。
// 只有官方给了 promotion 才算「有峰谷价」；promotion 在但折扣不可用时按原实现回落当前价。
func valleyPriceFactor(meta *jval) *float64 {
	promo := meta.get("promotion")
	if !promo.isObj() || len(promo.keys) == 0 {
		return nil
	}
	var peak *float64
	if p, ok := promo.get("before_promotion_price_factor").num(); ok && !promo.get("before_promotion_price_factor").isNull() {
		peak = &p
	} else if p, ok := meta.get("original_price_factor").num(); ok {
		peak = &p
	}
	if peak != nil {
		if d, ok := promo.get("discount_factor").num(); ok && d > 0 && d <= 1 {
			v := math.Round(*peak*d*10000) / 10000
			return &v
		}
	}
	if pf, ok := meta.get("price_factor").num(); ok {
		return &pf
	}
	return nil
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func copyModels(in []Model) []Model {
	out := make([]Model, len(in))
	for i, m := range in {
		m.Efforts = append([]string(nil), m.Efforts...)
		m.Aliases = append([]string(nil), m.Aliases...)
		if m.PriceFactorValley != nil {
			v := *m.PriceFactorValley
			m.PriceFactorValley = &v
		}
		out[i] = m
	}
	return out
}

// ---------------------------------------------------------------------------
// 对外 API
// ---------------------------------------------------------------------------

// Models 返回某区域的模型目录。
//
// cred 非 nil：内存缓存（5 分钟）过期或来源不是动态接口时，尝试官方动态接口
// /algo/api/v2/model/list（成功即落盘）；失败则依次回落磁盘缓存（7 天）与内置快照。
// cred 为 nil：只用内存 / 磁盘缓存与内置快照，**不打网络**。
// 任何一级都拿不到时内置快照兜底，因此 error 只在 ctx 已取消时返回。
func (c *Client) Models(ctx context.Context, realm string, cred *Cred) ([]Model, error) {
	realm = NormalizeRealm(realm)
	now := time.Now()
	if e := c.catalog.load(realm); e != nil && now.Sub(e.at) < catalogMemTTL {
		if e.source == sourceDynamic || cred == nil || now.Sub(e.dynAt) < catalogMemTTL {
			return copyModels(e.value.(*realmCatalog).models), nil
		}
	}
	if cred == nil {
		return copyModels(c.offlineCatalog(realm, true).models), nil
	}
	mu := c.catalog.lockFetch(realm)
	mu.Lock()
	defer mu.Unlock()
	// 等锁期间别的调用可能已拉取成功（或刚失败过）：再看一眼缓存。
	if e := c.catalog.load(realm); e != nil && time.Since(e.at) < catalogMemTTL &&
		(e.source == sourceDynamic || time.Since(e.dynAt) < catalogMemTTL) {
		return copyModels(e.value.(*realmCatalog).models), nil
	}
	rc, err := c.refreshDynamic(ctx, realm, *cred)
	if err == nil {
		return copyModels(rc.models), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	c.logf("model discovery (%s) failed, falling back to cache/snapshot: %v", realm, err)
	rc = c.offlineCatalog(realm, false)
	if e := c.catalog.load(realm); e != nil {
		e.dynAt = time.Now()
		c.catalog.store(realm, e)
	}
	return copyModels(rc.models), nil
}

// RefreshModels 强制走官方动态接口并更新内存与磁盘缓存；失败返回错误（缓存保持原状）。
func (c *Client) RefreshModels(ctx context.Context, realm string, cred Cred) ([]Model, error) {
	realm = NormalizeRealm(realm)
	mu := c.catalog.lockFetch(realm)
	mu.Lock()
	defer mu.Unlock()
	rc, err := c.refreshDynamic(ctx, realm, cred)
	if err != nil {
		if e := c.catalog.load(realm); e != nil {
			e.dynAt = time.Now()
			c.catalog.store(realm, e)
		}
		return nil, err
	}
	return copyModels(rc.models), nil
}

// LookupModel 把任一合法写法（官方名 / 内部 key / 展示 id「key (Name)」/ 小写别名 /
// 官方本地化名）解析成该区域目录里的模型。只读缓存与快照，不打网络。
func (c *Client) LookupModel(realm, name string) (Model, bool) {
	realm = NormalizeRealm(realm)
	rc := c.currentCatalog(realm)
	key := c.resolveUpstreamKey(name, realm)
	if i, ok := rc.byKey[key]; ok {
		return copyModels(rc.models[i : i+1])[0], true
	}
	// 原实现在解析不出时把原名当 key 透传；这里补一层大小写不敏感的 key 匹配
	// （只在原规则失败时生效，不改变任何能解析的写法）。
	low := strings.ToLower(strings.TrimSpace(key))
	for i, e := range rc.entries {
		if strings.ToLower(e.key) == low {
			return copyModels(rc.models[i : i+1])[0], true
		}
	}
	return Model{}, false
}

// ModelRealms 该名字在哪些区可用（cn 在前；只读缓存 / 快照，不打网络），供网关做两区调度。
//
// 同一上游 key 在两区可能是不同型号（原实现 auto_realm_for_model 的 mmodel 案例）：
// 当名字本身就是某个官方显示名时，只返回该区目录里显示名与之相同的区域，避免把
// 「MiniMax-M3」调度到 key 相同、型号不同的另一区。
func (c *Client) ModelRealms(name string) []string {
	pin := c.pinnedDisplayName(name)
	var out []string
	for _, r := range []string{RealmCN, RealmIntl} {
		m, ok := c.LookupModel(r, name)
		if !ok {
			continue
		}
		if pin != "" && strings.ToLower(m.ID) != pin {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// 解析（resolve_upstream_key）
// ---------------------------------------------------------------------------

// displayIndex 显示名（小写）→ key：当前目录（国内在前）+ 快照，先到先得。
func (c *Client) displayIndex() map[string]string {
	idx := map[string]string{}
	add := func(entries []catEntry) {
		for _, e := range entries {
			name := strings.ToLower(strings.TrimSpace(e.meta.get("display_name").str()))
			if name != "" {
				if _, ok := idx[name]; !ok {
					idx[name] = e.key
				}
			}
		}
	}
	add(staticModels(RealmCN))
	add(staticModels(RealmIntl))
	add(c.currentCatalog(RealmCN).entries)
	add(c.currentCatalog(RealmIntl).entries)
	return idx
}

func (c *Client) pinnedDisplayName(name string) string {
	m := strings.TrimSpace(name)
	if m == "" {
		return ""
	}
	low := strings.ToLower(m)
	tail := low
	if i := strings.LastIndex(low, " ("); i >= 0 && strings.HasSuffix(low, ")") {
		tail = strings.TrimRight(low[i+2:], ")")
	}
	idx := c.displayIndex()
	if _, ok := idx[tail]; ok {
		return tail
	}
	if _, ok := idx[low]; ok {
		return low
	}
	return ""
}

// resolveUpstreamKey 把客户端写的模型名解析为上游 key（规则与原实现逐条一致）：
// 空 → auto；「key (Name)」取 key；本区 key 命中；别名（原样 / 小写）；官方显示名；
// 官方本地化名；他区官方 key；带 "/" 前缀时取最后一段再试；都不中则原样返回。
func (c *Client) resolveUpstreamKey(model, realm string) string {
	if strings.TrimSpace(model) == "" {
		return "auto"
	}
	m := strings.TrimSpace(model)
	keys := map[string]bool{}
	for _, e := range c.currentCatalog(realm).entries {
		keys[e.key] = true
	}
	mKey := m
	if strings.Contains(m, " (") && strings.HasSuffix(m, ")") {
		head := strings.TrimSpace(m[:strings.LastIndex(m, " (")])
		if head != "" {
			mKey = head
		}
	}
	if keys[mKey] {
		return mKey
	}
	low := strings.ToLower(mKey)
	if v, ok := modelAliasMap[mKey]; ok {
		return v
	}
	if v, ok := modelAliasMap[low]; ok {
		return v
	}
	tail := low
	if i := strings.LastIndex(low, " ("); i >= 0 {
		tail = low[i+2:]
	}
	tail = strings.TrimRight(tail, ")")
	idx := c.displayIndex()
	if v, ok := idx[tail]; ok {
		return v
	}
	if v, ok := idx[low]; ok {
		return v
	}
	staticKeys := map[string]bool{}
	for _, e := range staticAll() {
		staticKeys[e.key] = true
	}
	for _, p := range officialLabels {
		if strings.ToLower(strings.TrimSpace(p.v)) == tail {
			if len(keys) == 0 || keys[p.k] || staticKeys[p.k] {
				return p.k
			}
		}
	}
	if len(keys) > 0 && staticKeys[mKey] {
		return mKey // 官方 key 但非本区域（跨区由调用方判断）
	}
	if i := strings.LastIndex(low, "/"); i >= 0 {
		part := low[i+1:]
		if v, ok := modelAliasMap[part]; ok {
			return v
		}
		if v, ok := idx[part]; ok {
			return v
		}
	}
	return mKey
}

// catalogMetaFor 按上游 key 取官方元数据：本区当前目录 → 国际版快照 → 国内版快照
// （catalog_meta_for 口径）；找不到返回空对象。
func (c *Client) catalogMetaFor(realm, key string) *jval {
	rc := c.currentCatalog(realm)
	if i, ok := rc.byKey[key]; ok {
		return rc.entries[i].meta
	}
	for _, list := range [][]catEntry{staticModels(RealmIntl), staticModels(RealmCN)} {
		for _, e := range list {
			if e.key == key {
				return e.meta
			}
		}
	}
	return &jval{k: jObj}
}

// ---------------------------------------------------------------------------
// 缓存层
// ---------------------------------------------------------------------------

// currentCatalog 当前可用的目录（不打网络）：内存（不论新旧）→ 磁盘 → 快照。
func (c *Client) currentCatalog(realm string) *realmCatalog {
	realm = NormalizeRealm(realm)
	if e := c.catalog.load(realm); e != nil {
		return e.value.(*realmCatalog)
	}
	return c.offlineCatalog(realm, true)
}

// offlineCatalog 不打网络地构造目录并写入内存缓存：新鲜内存（仅 preferMem）→ 磁盘 →
// 过期的动态内存 → 快照。
func (c *Client) offlineCatalog(realm string, preferMem bool) *realmCatalog {
	now := time.Now()
	old := c.catalog.load(realm)
	if preferMem && old != nil && now.Sub(old.at) < catalogMemTTL {
		return old.value.(*realmCatalog)
	}
	var dynAt time.Time
	if old != nil {
		dynAt = old.dynAt
	}
	if live := c.loadDiskCache(realm); len(live) > 0 {
		rc := newRealmCatalog(realm, mergeCatalog(live, realm))
		c.catalog.store(realm, &catalogCacheEntry{value: rc, at: now, source: sourceDisk, dynAt: dynAt})
		return rc
	}
	if old != nil && old.source == sourceDynamic {
		return old.value.(*realmCatalog)
	}
	rc := newRealmCatalog(realm, mergeCatalog(nil, realm))
	c.catalog.store(realm, &catalogCacheEntry{value: rc, at: now, source: sourceSnapshot, dynAt: dynAt})
	return rc
}

func (c *Client) refreshDynamic(ctx context.Context, realm string, cred Cred) (*realmCatalog, error) {
	live, err := c.fetchDynamicModels(ctx, realm, cred)
	now := time.Now()
	if err != nil {
		return nil, err
	}
	c.saveDiskCache(realm, live)
	rc := newRealmCatalog(realm, mergeCatalog(live, realm))
	c.catalog.store(realm, &catalogCacheEntry{value: rc, at: now, source: sourceDynamic, dynAt: now})
	c.logf("model discovery ok: %d chat models from %s", len(rc.models), realm)
	return rc, nil
}

// fetchDynamicModels COSY 签名拉取 /algo/api/v2/model/list（chat 场景）。
//
// 签名覆盖的 body 是 Encode("{}")，请求也必须**带同一串 body** 发出（服务端校验签名
// 与 body 一致，裸 GET 会 403）；主机按 c.Gateways 候选依次尝试（签名只覆盖 path，
// 换主机不影响签名）。
func (c *Client) fetchDynamicModels(ctx context.Context, realm string, cred Cred) ([]catEntry, error) {
	sess, err := c.Sessions.Get(cred)
	if err != nil {
		return nil, err
	}
	signBody := Encode([]byte("{}"))
	var lastErr error
	for _, host := range c.Gateways(realm) {
		rawURL := host + modelsPath
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, strings.NewReader(signBody))
		if err != nil {
			return nil, err
		}
		req.Header = sess.Headers(signBody, rawURL, HeaderOpts{Accept: "application/json"})
		fixUserAgent(req.Header)
		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.logf("model discovery failed on %s: %v", host, err)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
			c.logf("model discovery failed on %s: %v", host, lastErr)
			continue
		}
		if rerr != nil {
			lastErr = rerr
			continue
		}
		payload, err := parseJ(raw)
		if err != nil || !payload.isObj() {
			lastErr = fmt.Errorf("bad model list json: %v", err)
			continue
		}
		var out []catEntry
		if chat := payload.get("chat"); chat.isArr() {
			for _, m := range chat.arr {
				if !m.isObj() {
					continue
				}
				mid := m.get("key").str()
				if mid == "" {
					continue
				}
				// 官方条目全字段原样，仅补 id/name/display_name 兼容键（read_dynamic_models）。
				row := m.shallow()
				row.set("id", jStrV(mid))
				dn := m.get("display_name").str()
				if dn == "" {
					dn = mid
				}
				row.setDefault("name", jStrV(dn))
				row.setDefault("display_name", jStrV(dn))
				out = append(out, catEntry{key: mid, meta: row})
			}
		}
		if len(out) == 0 {
			lastErr = errors.New("model list returned no chat models")
			continue
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no gateway host")
	}
	return nil, lastErr
}

// fixUserAgent sign.go 以小写键写入 user-agent；net/http 只认规范键 "User-Agent"，
// 不挪过去会额外补发一个 "User-Agent: Go-http-client/1.1"（同一请求出现两个 UA）。
func fixUserAgent(h http.Header) {
	if ua, ok := h["user-agent"]; ok {
		delete(h, "user-agent")
		h["User-Agent"] = ua
	}
}

// ---- 磁盘缓存（与原实现 models-cache-<realm>.json 同格式） ----

func (c *Client) diskCachePath(realm string) string {
	if strings.TrimSpace(c.opts.DataDir) == "" {
		return ""
	}
	return filepath.Join(c.opts.DataDir, "models-cache-"+NormalizeRealm(realm)+".json")
}

func (c *Client) saveDiskCache(realm string, live []catEntry) {
	path := c.diskCachePath(realm)
	if path == "" {
		return
	}
	rows := make([]*jval, 0, len(live))
	for _, e := range live {
		rows = append(rows, jObjV("key", e.key, "meta", e.meta))
	}
	blob := jObjV("at", float64(time.Now().UnixNano())/1e9, "realm", NormalizeRealm(realm), "models", jArrV(rows...))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.logf("models cache write failed: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(blob.dumpPy()), 0o600); err != nil {
		c.logf("models cache write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		c.logf("models cache write failed: %v", err)
	}
}

func (c *Client) loadDiskCache(realm string) []catEntry {
	path := c.diskCachePath(realm)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	blob, err := parseJ(bytes.TrimSpace(raw))
	if err != nil || !blob.isObj() {
		return nil
	}
	at, _ := blob.get("at").num()
	if time.Since(time.Unix(0, int64(at*1e9))) > catalogDiskTTL {
		return nil
	}
	var out []catEntry
	if rows := blob.get("models"); rows.isArr() {
		for _, r := range rows.arr {
			mid := r.get("key").str()
			if mid == "" {
				continue
			}
			meta := r.get("meta")
			if !meta.isObj() {
				meta = &jval{k: jObj}
			}
			out = append(out, catEntry{key: mid, meta: meta})
		}
	}
	return out
}
