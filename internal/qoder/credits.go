package qoder

// 额度（GET /api/v2/quota/usage）与套餐（GET /api/v2/user/plan）。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：Account.fetch_credits / _note_quota_anomaly / _other_realm_quota_hint /
// fetch_plan / effective_remain / shared_unlimited / next_checkin_window。
//
// 额度口径（与原实现逐项一致，单元测试用原实现的解析结果做向量对照）：
//
//	Remain / Used / Size  = userQuota（套餐）+ addOnQuota（赠送 / 签到）两项之和（截断取整）；
//	RemainEffective       = Remain + 可用且非无限的共享池 remain（issue #39：团队账号套餐用满、
//	                        共享池仍有余额时，只看 Remain 会把可用账号误判为耗尽）；
//	Shared                = orgResourcePackage 等共享池（cap=-1 是「不设上限」哨兵，见 Unlimited）。
//
// 退化响应守卫（#25-d）：上游对失效 / 无权益凭证会回一个「全 0 + isQuotaExceeded=true」的体，
// 它不是「余额 0」而是「这次没拿到可信余额」。FetchCredits 对这种体返回 Code=QUOTA_DEGENERATE
// 的错误，调用方必须**保留旧快照**（不要拿 0 覆盖健康账本），并可记录 Error.Body 供事后排查。

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"
)

// CreditPackage 额度明细中的一项（基础额度 / 赠送/签到额度 / 共享额度）。
// json 标签与原实现账号文件 credits.packages[] 的键名一致，便于沿用原账号文件格式落盘。
type CreditPackage struct {
	Name        string   `json:"name"`
	Remain      float64  `json:"remain"`
	Used        float64  `json:"used"`
	Size        float64  `json:"size"`
	ExpiresAt   int64    `json:"expires_at,omitempty"`   // 包级到期（上游给了才有；Unix 秒）
	DaysLeft    *float64 `json:"days_left,omitempty"`    // 上游 daysLeft（给了才有）
	NoExpiry    *bool    `json:"no_expiry,omitempty"`    // 上游 noExpiry（给了才有）
	PackageCode string   `json:"package_code,omitempty"` // 上游 packageCode（给了才有）
	// 以下三项只出现在「共享额度」包上（原实现 {"name": "共享额度", **shared}）。
	Available *bool  `json:"available,omitempty"`
	Unlimited bool   `json:"unlimited,omitempty"`
	Source    string `json:"source,omitempty"`
}

// SharedCredits 组织共享池（团队 / 企业账号）。
type SharedCredits struct {
	Remain float64 `json:"remain"`
	Used   float64 `json:"used"`
	Size   float64 `json:"size"` // cap（缺失或为 0 时用 total）；无限池为 -1
	// Available 缺键按可用（「没声明不可用」）；"false" / 0 等防御性解读为不可用。
	Available bool `json:"available"`
	// Unlimited cap=-1：不设上限。此时 Remain 只是占位数（同组织各账号回显一致），
	// 展示与护栏都不应把它当余额；也不计入 RemainEffective。
	Unlimited   bool     `json:"unlimited,omitempty"`
	Source      string   `json:"source"` // 上游字段名（orgResourcePackage / sharedQuota / ...）
	ExpiresAt   int64    `json:"expires_at,omitempty"`
	DaysLeft    *float64 `json:"days_left,omitempty"`
	NoExpiry    *bool    `json:"no_expiry,omitempty"`
	PackageCode string   `json:"package_code,omitempty"`
}

// Credits 一次额度快照。json 标签与原实现账号文件 credits 字段一致（updated_at 例外：
// 原实现是浮点 Unix 秒，这里是 time.Time 的 RFC3339 文本）。
type Credits struct {
	Remain          float64 `json:"remain"`
	RemainEffective float64 `json:"remain_effective"` // 含可用共享池的「有效余额」——额度判定一律看它
	Used            float64 `json:"used"`
	Size            float64 `json:"size"`
	Exceeded        bool    `json:"exceeded"`  // 上游 isQuotaExceeded（Python bool() 口径）
	UsagePct        float64 `json:"usage_pct"` // 上游 totalUsagePercentage（缺失为 0）
	// ExpiresAt 上游顶层 expiresAt（原样归一为秒，不映射到包级——归属未证实，误贴会把按月重置的
	// 额度算成临期）。国内版常见 253402214400（9999-12-31，「永不过期」占位）。
	ExpiresAt int64           `json:"expires_at"`
	Shared    *SharedCredits  `json:"shared"`
	Packages  []CreditPackage `json:"packages"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// SharedUnlimited 共享池是否「不设上限」（issue #67）。
func (c Credits) SharedUnlimited() bool { return c.Shared != nil && c.Shared.Unlimited }

// HasBalance 是否还有可用额度：有效余额 > 0，或共享池不设上限。
// 原实现 revive_balance_cooldown 的判据（402 停放只在额度刷新看到它为真时解除）。
func (c Credits) HasBalance() bool { return c.SharedUnlimited() || c.RemainEffective > 0 }

// FetchCredits 查询额度（只读）。
//
// 401/403 时按原实现先用桌面端同款头重试一次（官方客户端用的就是这套）；仍失败再探测
// 「令牌是否属于另一个区域」（同一 token 打另一区的 quota 能成功 = 账号 realm 标错了），
// 把可操作的提示拼进 Error.Msg。探测只发只读 GET。
func (c *Client) FetchCredits(ctx context.Context, cred Cred) (Credits, *Error) {
	realm := NormalizeRealm(cred.Realm)
	u := oaURL(realm, oaPathQuota)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: u, header: oaPlainHeaders(cred),
		attempts: 3, timeout: 30 * time.Second})
	if err != nil {
		if (err.Status != http.StatusUnauthorized && err.Status != http.StatusForbidden) || err.Kind == KindSessionDead {
			return Credits{}, err
		}
		h, _ := c.oaDesktopHeaders(ctx, cred)
		resp2, err2 := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: u, header: h,
			attempts: 3, timeout: 30 * time.Second})
		if err2 != nil {
			out := *err2
			hint := c.otherRealmQuotaHint(ctx, cred)
			out.Msg = "HTTP " + strconv.Itoa(err.Status) + " " + oaCut(err2.Msg, 160)
			if hint != "" {
				out.Msg += " ｜ " + hint
			}
			return Credits{}, &out
		}
		resp = resp2
	}
	return parseQuotaBody(resp.body, time.Now())
}

// otherRealmQuotaHint issue #40 探测：令牌是否属于**另一个区域**。返回可读提示；不是该情形返回空串。
func (c *Client) otherRealmQuotaHint(ctx context.Context, cred Cred) string {
	other := RealmIntl
	if NormalizeRealm(cred.Realm) == RealmIntl {
		other = RealmCN
	}
	probe := cred
	probe.Realm = other
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: oaURL(other, oaPathQuota),
		header: oaPlainHeaders(probe), attempts: 1, timeout: 20 * time.Second})
	if err != nil {
		return ""
	}
	q, ok := oaObject(resp.body)
	if !ok {
		return ""
	}
	_, hasUser := q["userQuota"]
	_, hasAddOn := q["addOnQuota"]
	if !hasUser && !hasAddOn {
		return ""
	}
	name := ConfigFor(other).Name
	return "该账号的令牌在" + name + "可用，本区 403 很可能是**区域标记错误**：请重新导入该账号（或把 realm 改成 " + other + "）"
}

// sharedQuotaKeys 共享池字段名候选（按原实现顺序；实测团队账号是 orgResourcePackage）。
var sharedQuotaKeys = []string{"orgResourcePackage", "sharedQuota", "teamQuota", "poolQuota", "sharedCredits", "sharedCredit"}

// parseQuotaBody 解析 quota/usage 响应体（纯函数，与原实现 fetch_credits 的解析段一一对应）。
func parseQuotaBody(raw []byte, now time.Time) (Credits, *Error) {
	v, derr := oaDecode(raw)
	q, isObj := v.(map[string]any)
	if derr != nil {
		return Credits{}, &Error{Kind: KindServer, Code: "QUOTA_DEGENERATE",
			Msg:  "invalid quota body: " + derr.Error(),
			Body: oaCut(string(raw), 400)}
	}
	if !isObj {
		return Credits{}, quotaDegenerate("non-dict response", raw)
	}
	uq, uqObj := q["userQuota"].(map[string]any)
	aq, aqObj := q["addOnQuota"].(map[string]any)
	if !uqObj {
		uq = oaMap(nil)
	}
	if !aqObj {
		aq = oaMap(nil)
	}
	num := func(m map[string]any, k string) float64 { return oaNum(m[k]) }
	remain := math.Trunc(num(uq, "remaining") + num(aq, "remaining"))
	used := math.Trunc(num(uq, "used") + num(aq, "used"))
	size := math.Trunc(num(uq, "total") + num(aq, "total"))
	// 判据保守：两个 quota 都缺；或 exceeded=true 且 remain=0 且 size=0（「从没有过额度」）。
	// 真·耗尽的特征是 size>0 而 remain=0，会被放行；不依赖 expiresAt=253402214400（健康号也会返回它）。
	bothMissing := !uqObj && !aqObj
	if bothMissing {
		return Credits{}, quotaDegenerate("both quota objects missing", raw)
	}
	if oaTruthy(q["isQuotaExceeded"]) && remain == 0 && size == 0 {
		return Credits{}, quotaDegenerate("exceeded=true with zero size/remain", raw)
	}

	var shared *SharedCredits
	for _, key := range sharedQuotaKeys {
		src, present := q[key]
		if !present {
			continue
		}
		if m, ok := src.(map[string]any); ok {
			sz := num(m, "cap")
			if sz == 0 {
				sz = num(m, "total")
			}
			avail, hasAvail := m["available"]
			if !hasAvail {
				avail = true
			}
			shared = &SharedCredits{
				Remain:    math.Trunc(num(m, "remaining")),
				Used:      math.Trunc(num(m, "used")),
				Size:      math.Trunc(sz),
				Available: oaTruthyField(avail, true),
				Source:    key,
			}
			ex := expiryOf(m)
			shared.ExpiresAt, shared.DaysLeft, shared.NoExpiry, shared.PackageCode = ex.expiresAt, ex.daysLeft, ex.noExpiry, ex.packageCode
			if shared.Size < 0 {
				shared.Unlimited = true
			}
			break
		}
		if n, ok := oaNumber(src); ok {
			shared = &SharedCredits{Remain: math.Trunc(n), Size: math.Trunc(n), Available: true, Source: key}
			break
		}
	}

	base := CreditPackage{Name: "基础额度", Remain: math.Trunc(num(uq, "remaining")),
		Used: math.Trunc(num(uq, "used")), Size: math.Trunc(num(uq, "total"))}
	expiryOf(uq).apply(&base)
	addon := CreditPackage{Name: "赠送/签到额度", Remain: math.Trunc(num(aq, "remaining")),
		Used: math.Trunc(num(aq, "used")), Size: math.Trunc(num(aq, "total"))}
	expiryOf(aq).apply(&addon)
	pkgs := []CreditPackage{base, addon}
	effective := remain
	if shared != nil {
		avail := shared.Available
		pkgs = append(pkgs, CreditPackage{Name: "共享额度", Remain: shared.Remain, Used: shared.Used,
			Size: shared.Size, ExpiresAt: shared.ExpiresAt, DaysLeft: shared.DaysLeft, NoExpiry: shared.NoExpiry,
			PackageCode: shared.PackageCode, Available: &avail, Unlimited: shared.Unlimited, Source: shared.Source})
		if shared.Available && !shared.Unlimited {
			effective += shared.Remain
		}
	}
	return Credits{
		Remain:          remain,
		RemainEffective: math.Trunc(effective),
		Used:            used,
		Size:            size,
		Exceeded:        oaTruthy(q["isQuotaExceeded"]),
		UsagePct:        oaNum(q["totalUsagePercentage"]),
		ExpiresAt:       normalizeEpoch(q["expiresAt"]),
		Shared:          shared,
		Packages:        pkgs,
		UpdatedAt:       now,
	}, nil
}

// quotaDegenerate 退化体错误（调用方保留旧快照；Body 是原文前 400 字符，对应原实现 lastQuotaAnomaly.raw）。
func quotaDegenerate(reason string, raw []byte) *Error {
	return &Error{Kind: KindServer, Code: "QUOTA_DEGENERATE",
		Msg:  "degenerate quota body (" + reason + "); kept previous snapshot",
		Body: oaCut(string(raw), 400)}
}

// oaNumber 判断 JSON 值是不是数字（bool 不算——Python 里 bool 是 int 子类，原实现显式排除）。
func oaNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		if n, ok := v.(interface{ Float64() (float64, error) }); ok {
			f, err := n.Float64()
			return f, err == nil
		}
		return 0, false
	}
}

// pkgExpiry 包级到期信息的防御性透传（原实现 _expiry_of：存在才写、不臆造）。
type pkgExpiry struct {
	expiresAt   int64
	daysLeft    *float64
	noExpiry    *bool
	packageCode string
}

func (e pkgExpiry) apply(p *CreditPackage) {
	p.ExpiresAt, p.DaysLeft, p.NoExpiry, p.PackageCode = e.expiresAt, e.daysLeft, e.noExpiry, e.packageCode
}

func expiryOf(src map[string]any) pkgExpiry {
	var out pkgExpiry
	if src == nil {
		return out
	}
	for _, k := range []string{"expiresAt", "expireAt", "expires_at"} {
		if v, ok := src[k]; ok && v != nil {
			out.expiresAt = normalizeEpoch(v)
			break
		}
	}
	for _, k := range []string{"daysLeft", "days_left"} {
		if v, ok := src[k]; ok && v != nil {
			d := oaNum(v)
			out.daysLeft = &d
			break
		}
	}
	if v, ok := src["noExpiry"]; ok && v != nil {
		b := oaTruthy(v)
		out.noExpiry = &b
	}
	out.packageCode = oaStr(src["packageCode"])
	if out.packageCode == "" {
		out.packageCode = oaStr(src["package_code"])
	}
	return out
}

// FetchPlan 查询套餐名（如 "Pro Trial"；只读）。取 plan_tier_name，缺失退回 user_type。
func (c *Client) FetchPlan(ctx context.Context, cred Cred) (string, *Error) {
	realm := NormalizeRealm(cred.Realm)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: oaURL(realm, oaPathPlan),
		header: oaPlainHeaders(cred), attempts: 1, timeout: 15 * time.Second})
	if err != nil {
		return "", err
	}
	p, ok := oaObject(resp.body)
	if !ok {
		return "", &Error{Kind: KindServer, Status: resp.status, Msg: "plan: unexpected body",
			Body: truncate(string(resp.body), oaBodyKeep)}
	}
	name := oaStr(p["plan_tier_name"])
	if name == "" {
		name = oaStr(p["user_type"])
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// 每日刷新窗口（UTC+8 10:00）
// ---------------------------------------------------------------------------

// CheckinWindowHourUTC8 官方每日活动 / 额度刷新时刻（UTC+8 的小时）。
const CheckinWindowHourUTC8 = 10

// checkinZoneUTC8 固定 UTC+8。窗口**必须**按固定 UTC+8 计算——容器里常是 UTC，按本地时区渲染会差 8 小时。
var checkinZoneUTC8 = time.FixedZone("UTC+8", 8*3600)

// NextCheckinWindow 下一个「每日 10:00（UTC+8）」：now 早于当日 10:00 → 当日 10:00；
// 到达或晚于 10:00（含刚领取成功）→ 次日 10:00（「到点即算下一轮」，不返回已到点的时刻）。
// 也是原实现 402 余额停放的截止时刻口径。
func NextCheckinWindow(now time.Time) time.Time {
	n8 := now.In(checkinZoneUTC8)
	b := time.Date(n8.Year(), n8.Month(), n8.Day(), CheckinWindowHourUTC8, 0, 0, 0, checkinZoneUTC8)
	if !n8.Before(b) {
		b = b.AddDate(0, 0, 1)
	}
	return b
}

// CheckinWindowNote 窗口的可读文本（形如 "10-05 10:00（UTC+8）"，与原实现 next_available_note 一致）。
func CheckinWindowNote(t time.Time) string {
	return t.In(checkinZoneUTC8).Format("01-02 15:04") + "（UTC+8）"
}
