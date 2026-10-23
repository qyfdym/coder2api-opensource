package kiro

// REST 接口：额度（getUsageLimits）、模型目录（ListAvailableModels）、数据面 profile
// （ListAvailableProfiles）。移植自 Kiro-Go proxy/kiro_api.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const restBase = "https://codewhisperer.us-east-1.amazonaws.com"

var (
	regionPattern  = regexp.MustCompile(`^[a-z]{2}(?:-[a-z0-9]+)+-[0-9]+$`)
	accountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	profilePattern = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]+$`)
	// profileRegions 已知的 Kiro 数据面区域（显式列表：不把持久化的认证区域原样拼进出站主机名）。
	profileRegions = []string{"us-east-1", "eu-central-1"}
)

// Kiro IDE 1.3.x 内置的固定 profile：Builder ID 与社交登录账号不查 ListAvailableProfiles，直接用这两个 ARN。
//
// 出站版本号是 1.3.x 时，推理请求（"profileArn is required for this request"）和模型目录
// （400）都必须带 profileArn；0.11.x 版本号不带也能用，Kiro-Go 时代的代码因此没有这一步。
const (
	builderIDProfileArn = "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX"
	socialProfileArn    = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
)

// DefaultProfileArn 账号类型对应的固定 profileArn（Builder ID / 社交登录）；企业账号与 API Key 返回空，
// 企业账号要用 ListAvailableProfiles 查（见 ResolveProfileArn）。
func DefaultProfileArn(cred Cred) string {
	switch {
	case cred.IsAPIKey():
		return ""
	case strings.EqualFold(strings.TrimSpace(cred.Provider), "BuilderId"):
		return builderIDProfileArn
	case strings.EqualFold(strings.TrimSpace(cred.AuthMethod), "social"):
		return socialProfileArn
	}
	return ""
}

// effectiveProfileArn 请求实际带的 profileArn：账号自带的优先（刷新响应 / profile 列表里取得的），
// 否则用账号类型的固定值；API Key 账号不带。
func effectiveProfileArn(cred Cred) string {
	if cred.IsAPIKey() {
		return ""
	}
	if p := strings.TrimSpace(cred.ProfileArn); p != "" {
		return p
	}
	return DefaultProfileArn(cred)
}

// ParseProfileArn 校验 profileArn（arn:aws:codewhisperer:<region>:<12位账号>:profile/<id>），返回区域。
func ParseProfileArn(arn string) (canonical, region string, ok bool) {
	canonical = strings.TrimSpace(arn)
	parts := strings.SplitN(canonical, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "codewhisperer" ||
		!regionPattern.MatchString(parts[3]) || !accountPattern.MatchString(parts[4]) ||
		!strings.HasPrefix(parts[5], "profile/") {
		return "", "", false
	}
	if !profilePattern.MatchString(strings.TrimPrefix(parts[5], "profile/")) {
		return "", "", false
	}
	return canonical, parts[3], true
}

// dataRegion 数据面区域：profileArn 优先（认证区域可能与 profile 区域不同）；
// API Key 账号用其 Region；其余 us-east-1。
func dataRegion(cred Cred, profileArn string) string {
	if _, r, ok := ParseProfileArn(profileArn); ok {
		return r
	}
	if _, r, ok := ParseProfileArn(cred.ProfileArn); ok {
		return r
	}
	if cred.IsAPIKey() {
		if r := strings.ToLower(strings.TrimSpace(cred.Region)); regionPattern.MatchString(r) {
			return r
		}
	}
	return defaultRegion
}

// regionalize 把写死 us-east-1 的入口改到目标区域：Amazon Q 是分区的（q.<region>），
// CodeWhisperer REST 主机只有 us-east-1，其它区域由 q.<region> 服务。
func regionalize(rawURL, region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == defaultRegion || !regionPattern.MatchString(region) {
		return rawURL
	}
	host := "q." + region + ".amazonaws.com"
	return strings.NewReplacer("q.us-east-1.amazonaws.com", host, "codewhisperer.us-east-1.amazonaws.com", host).Replace(rawURL)
}

// restDo 发一个 REST 请求并读回 JSON（非 2xx 返回分类后的 *Error）。
func (c *Client) restDo(ctx context.Context, cred Cred, method, rawURL string, body any) ([]byte, *Error) {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, &Error{Kind: KindBadRequest, Msg: err.Error()}
	}
	c.setRESTHeaders(req, cred)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.rest.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, newError(resp.StatusCode, string(raw), resp.Header.Get("Retry-After"))
	}
	return raw, nil
}

func withProfileQuery(rawURL, profileArn string) string {
	if p := strings.TrimSpace(profileArn); p != "" {
		return rawURL + "&profileArn=" + url.QueryEscape(p)
	}
	return rawURL
}

// ---------------------------------------------------------------------------
// 额度
// ---------------------------------------------------------------------------

// Usage 账号额度（单位 Kiro Credits）。
type Usage struct {
	Plan      string // FREE / PRO / PRO_PLUS / POWER
	PlanTitle string
	// Current / Limit 订阅周期内的基础额度。
	Current float64
	Limit   float64
	// Trial* 试用额度（新号的赠送额度，ACTIVE 时计入可用）。
	TrialCurrent   float64
	TrialLimit     float64
	TrialStatus    string
	TrialExpiresAt int64
	// Bonuses 活动 / 补偿等附加额度。
	Bonuses     []Bonus
	NextResetAt int64 // 下次重置（Unix 秒）
	Email       string
	UserID      string
	// 超额（额度用完后按量计费）：状态 ENABLED / DISABLED（空 = 上游没给）、订阅是否支持、
	// 美元上限 / 单价 / 本周期已产生的超额费用。
	OverageStatus     string
	OverageCapability string
	OverageCap        float64
	OverageRate       float64
	CurrentOverages   float64
}

// Bonus 一笔附加额度。
type Bonus struct {
	Name      string
	Current   float64
	Limit     float64
	ExpiresAt int64
	Status    string
}

func (b Bonus) active(now time.Time) bool {
	if s := strings.ToUpper(b.Status); s != "" && s != "ACTIVE" {
		return false
	}
	return b.ExpiresAt == 0 || time.Unix(b.ExpiresAt, 0).After(now)
}

func (u Usage) trialActive() bool { return strings.EqualFold(u.TrialStatus, "ACTIVE") }

// Totals 可用额度汇总：(剩余, 总额)。基础额度 + 生效中的试用 + 生效中的附加额度。
func (u Usage) Totals(now time.Time) (remain, total float64) {
	remain = math.Max(0, u.Limit-u.Current)
	total = u.Limit
	if u.trialActive() && u.TrialLimit > 0 {
		remain += math.Max(0, u.TrialLimit-u.TrialCurrent)
		total += u.TrialLimit
	}
	for _, b := range u.Bonuses {
		if b.active(now) && b.Limit > 0 {
			remain += math.Max(0, b.Limit-b.Current)
			total += b.Limit
		}
	}
	return remain, total
}

type usageBreakdown struct {
	ResourceType          string   `json:"resourceType"`
	CurrentUsage          float64  `json:"currentUsage"`
	UsageLimit            float64  `json:"usageLimit"`
	CurrentUsagePrecision *float64 `json:"currentUsageWithPrecision"`
	UsageLimitPrecision   *float64 `json:"usageLimitWithPrecision"`
	OverageCap            float64  `json:"overageCap"`
	OverageRate           float64  `json:"overageRate"`
	CurrentOverages       float64  `json:"currentOverages"`
	FreeTrialInfo         *struct {
		CurrentUsage          float64     `json:"currentUsage"`
		UsageLimit            float64     `json:"usageLimit"`
		CurrentUsagePrecision *float64    `json:"currentUsageWithPrecision"`
		UsageLimitPrecision   *float64    `json:"usageLimitWithPrecision"`
		FreeTrialStatus       string      `json:"freeTrialStatus"`
		FreeTrialExpiry       json.Number `json:"freeTrialExpiry"`
	} `json:"freeTrialInfo"`
	Bonuses []struct {
		BonusCode    string      `json:"bonusCode"`
		DisplayName  string      `json:"displayName"`
		CurrentUsage float64     `json:"currentUsage"`
		UsageLimit   float64     `json:"usageLimit"`
		ExpiresAt    json.Number `json:"expiresAt"`
		Status       string      `json:"status"`
	} `json:"bonuses"`
}

type usageLimitsResponse struct {
	UsageBreakdownList []usageBreakdown `json:"usageBreakdownList"`
	NextDateReset      json.Number      `json:"nextDateReset"`
	SubscriptionInfo   *struct {
		SubscriptionName  string `json:"subscriptionName"`
		SubscriptionTitle string `json:"subscriptionTitle"`
		SubscriptionType  string `json:"subscriptionType"`
		OverageCapability string `json:"overageCapability"`
	} `json:"subscriptionInfo"`
	OverageConfiguration *struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	UserInfo *struct {
		Email  string `json:"email"`
		UserID string `json:"userId"`
	} `json:"userInfo"`
}

func numberUnix(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		if v > 1e12 { // 毫秒
			v /= 1000
		}
		return v
	}
	if f, err := n.Float64(); err == nil {
		v := int64(f)
		if v > 1e12 {
			v /= 1000
		}
		return v
	}
	return 0
}

func prec(p *float64, plain float64) float64 {
	if p != nil {
		return *p
	}
	return plain
}

// UsageLimits 查询额度（顺带拿到邮箱 / userId / 套餐）。
func (c *Client) UsageLimits(ctx context.Context, cred Cred) (Usage, *Error) {
	u := regionalize(restBase+"/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true",
		dataRegion(cred, ""))
	if !cred.IsAPIKey() {
		u = withProfileQuery(u, effectiveProfileArn(cred))
	}
	raw, err := c.restDo(ctx, cred, http.MethodGet, u, nil)
	if err != nil {
		return Usage{}, err
	}
	var r usageLimitsResponse
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return Usage{}, &Error{Kind: KindServer, Msg: "getUsageLimits: bad JSON", Body: truncate(string(raw), 300)}
	}
	var out Usage
	if r.UserInfo != nil {
		out.Email, out.UserID = r.UserInfo.Email, r.UserInfo.UserID
	}
	if s := r.SubscriptionInfo; s != nil {
		name := firstNonEmptyStr(s.SubscriptionTitle, s.SubscriptionName, s.SubscriptionType)
		out.Plan = parsePlan(name + " " + s.SubscriptionType)
		out.PlanTitle = firstNonEmptyStr(s.SubscriptionTitle, s.SubscriptionName)
		out.OverageCapability = strings.TrimSpace(s.OverageCapability)
	}
	if oc := r.OverageConfiguration; oc != nil {
		out.OverageStatus = strings.ToUpper(strings.TrimSpace(oc.OverageStatus))
	}
	out.NextResetAt = numberUnix(r.NextDateReset)
	// 取 AGENTIC_REQUEST（对话额度）那一栏；没有标类型时取第一栏。
	var b *usageBreakdown
	for i := range r.UsageBreakdownList {
		if strings.EqualFold(r.UsageBreakdownList[i].ResourceType, "AGENTIC_REQUEST") {
			b = &r.UsageBreakdownList[i]
			break
		}
	}
	if b == nil && len(r.UsageBreakdownList) > 0 {
		b = &r.UsageBreakdownList[0]
	}
	if b != nil {
		out.Current = prec(b.CurrentUsagePrecision, b.CurrentUsage)
		out.Limit = prec(b.UsageLimitPrecision, b.UsageLimit)
		out.OverageCap, out.OverageRate, out.CurrentOverages = b.OverageCap, b.OverageRate, b.CurrentOverages
		if t := b.FreeTrialInfo; t != nil {
			out.TrialCurrent = prec(t.CurrentUsagePrecision, t.CurrentUsage)
			out.TrialLimit = prec(t.UsageLimitPrecision, t.UsageLimit)
			out.TrialStatus = t.FreeTrialStatus
			out.TrialExpiresAt = numberUnix(t.FreeTrialExpiry)
		}
		for _, bo := range b.Bonuses {
			out.Bonuses = append(out.Bonuses, Bonus{
				Name: firstNonEmptyStr(bo.DisplayName, bo.BonusCode), Current: bo.CurrentUsage, Limit: bo.UsageLimit,
				ExpiresAt: numberUnix(bo.ExpiresAt), Status: bo.Status,
			})
		}
	}
	return out, nil
}

// SetOverage 打开 / 关闭账号的超额开关（POST setUserPreference），再读一次额度返回最新状态。
// 打开后额度用完不会被上游拒绝，超出部分按量计费（费用上限与单价见返回的 Usage）。
func (c *Client) SetOverage(ctx context.Context, cred Cred, enabled bool) (Usage, *Error) {
	status := "DISABLED"
	if enabled {
		status = "ENABLED"
	}
	body := map[string]any{"overageConfiguration": map[string]string{"overageStatus": status}}
	if !cred.IsAPIKey() {
		arn := strings.TrimSpace(cred.ProfileArn)
		if arn == "" {
			var err *Error
			if arn, err = c.ResolveProfileArn(ctx, cred); err != nil {
				return Usage{}, err
			}
		}
		if arn != "" {
			body["profileArn"] = arn
		}
	}
	u := regionalize("https://q.us-east-1.amazonaws.com/setUserPreference", dataRegion(cred, ""))
	if _, err := c.restDo(ctx, cred, http.MethodPost, u, body); err != nil {
		return Usage{}, err
	}
	// 上游偶尔滞后：以刚设置的值为准；读失败也不算失败（设置已经成功）。
	usage, err := c.UsageLimits(ctx, cred)
	if err != nil {
		return Usage{OverageStatus: status}, nil
	}
	usage.OverageStatus = status
	return usage, nil
}

func parsePlan(raw string) string {
	u := strings.ToUpper(raw)
	switch {
	case strings.Contains(u, "PRO_PLUS") || strings.Contains(u, "PROPLUS") || strings.Contains(u, "PRO+"):
		return "PRO_PLUS"
	case strings.Contains(u, "PRO_MAX") || strings.Contains(u, "PRO MAX") || strings.Contains(u, "PROMAX"):
		return "PRO_MAX" // 必须排在 PRO 之前，否则会被吞成 PRO
	case strings.Contains(u, "POWER"):
		return "POWER"
	case strings.Contains(u, "STUDENT"):
		return "STUDENT"
	case strings.Contains(u, "PRO"):
		return "PRO"
	default:
		return "FREE"
	}
}

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

// ListModels 拉账号可用的模型（ListAvailableModels）。
func (c *Client) ListModels(ctx context.Context, cred Cred) ([]Model, *Error) {
	u := regionalize(restBase+"/ListAvailableModels?origin=AI_EDITOR&maxResults=50", dataRegion(cred, ""))
	if !cred.IsAPIKey() {
		// 1.3.x 版本号下必须带 profileArn（缺了返回 400），见 DefaultProfileArn。
		u = withProfileQuery(u, effectiveProfileArn(cred))
	}
	raw, err := c.restDo(ctx, cred, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Models []struct {
			ModelID        string   `json:"modelId"`
			ModelName      string   `json:"modelName"`
			Description    string   `json:"description"`
			InputTypes     []string `json:"supportedInputTypes"`
			RateMultiplier float64  `json:"rateMultiplier"`
			TokenLimits    *struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			} `json:"tokenLimits"`
		} `json:"models"`
	}
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return nil, &Error{Kind: KindServer, Msg: "ListAvailableModels: bad JSON", Body: truncate(string(raw), 300)}
	}
	out := make([]Model, 0, len(r.Models))
	for _, m := range r.Models {
		if strings.TrimSpace(m.ModelID) == "" {
			continue
		}
		mm := Model{ID: m.ModelID, Name: m.ModelName, Description: m.Description, RateMultiplier: m.RateMultiplier}
		if m.TokenLimits != nil {
			mm.MaxInput, mm.MaxOutput = m.TokenLimits.MaxInputTokens, m.TokenLimits.MaxOutputTokens
		}
		for _, t := range m.InputTypes {
			if lt := strings.ToLower(t); strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
				mm.Vision = true
			}
		}
		out = append(out, mm)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 数据面 profile
// ---------------------------------------------------------------------------

const profileUnsupportedCooldown = 24 * time.Hour

// Profile 一个数据面 profile。
type Profile struct {
	ARN    string
	Name   string
	Region string
}

// ResolveProfileArn 为缺 profileArn 的账号找一个可用 profile：Builder ID / 社交登录直接用固定值
// （不打网络），企业账号按候选区域依次查 ListAvailableProfiles。
// 其它 Builder ID 形态的账号查 ListAvailableProfiles 会得到 403（不受支持），记下来 24 小时内不再查，
// 返回 ("", nil)。
func (c *Client) ResolveProfileArn(ctx context.Context, cred Cred) (string, *Error) {
	if p := effectiveProfileArn(cred); p != "" {
		return p, nil
	}
	if cred.IsAPIKey() {
		return "", nil
	}
	if until, ok := c.profileSuppress.Load(cred.UID); ok && time.Now().Before(until.(time.Time)) {
		return "", nil
	}
	var last *Error
	for _, region := range c.profileCandidates(cred) {
		profiles, err := c.listProfiles(ctx, cred, region)
		if err != nil {
			if strings.EqualFold(cred.Provider, "BuilderId") && err.Status == http.StatusForbidden &&
				strings.Contains(err.Body, "Builder ID is not supported") {
				c.profileSuppress.Store(cred.UID, time.Now().Add(profileUnsupportedCooldown))
				return "", nil
			}
			last = err
			continue
		}
		if len(profiles) > 0 {
			return profiles[0].ARN, nil
		}
	}
	if last != nil {
		return "", last
	}
	return "", nil
}

// ListProfiles 列出账号在各候选区域下能用的全部 profile（企业账号可能有多个，需要用户挑一个）。
// 全部区域都失败才返回错误；Builder ID 不支持 profile，直接返回空列表。
func (c *Client) ListProfiles(ctx context.Context, cred Cred) ([]Profile, *Error) {
	if cred.IsAPIKey() || strings.EqualFold(cred.Provider, "BuilderId") {
		return nil, nil
	}
	var out []Profile
	seen := map[string]bool{}
	var last *Error
	ok := false
	for _, region := range c.profileCandidates(cred) {
		ps, err := c.listProfiles(ctx, cred, region)
		if err != nil {
			last = err
			continue
		}
		ok = true
		for _, p := range ps {
			if !seen[p.ARN] {
				seen[p.ARN] = true
				out = append(out, p)
			}
		}
	}
	if !ok && last != nil {
		return nil, last
	}
	return out, nil
}

func (c *Client) profileCandidates(cred Cred) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		r = strings.ToLower(strings.TrimSpace(r))
		if regionPattern.MatchString(r) && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if _, r, ok := ParseProfileArn(cred.ProfileArn); ok {
		add(r)
	}
	for _, r := range profileRegions {
		add(r)
	}
	return out
}

func (c *Client) listProfiles(ctx context.Context, cred Cred, region string) ([]Profile, *Error) {
	endpoint := regionalize(restBase+"/ListAvailableProfiles", region)
	var out []Profile
	seen := map[string]bool{}
	next := ""
	for page := 0; page < 20; page++ {
		// 请求体里只有 nextToken：带 maxResults 会被上游以 REQUEST_BODY_INVALID 拒绝（真机验证）。
		body := map[string]any{}
		if next != "" {
			body["nextToken"] = next
		}
		raw, err := c.restDo(ctx, cred, http.MethodPost, endpoint, body)
		if err != nil {
			return nil, err
		}
		var r struct {
			Profiles []struct {
				ARN  string `json:"arn"`
				Name string `json:"profileName"`
			} `json:"profiles"`
			NextToken string `json:"nextToken"`
		}
		if jerr := json.Unmarshal(raw, &r); jerr != nil {
			return nil, &Error{Kind: KindServer, Msg: "ListAvailableProfiles: bad JSON", Body: truncate(string(raw), 300)}
		}
		for _, p := range r.Profiles {
			arn, reg, ok := ParseProfileArn(p.ARN)
			if !ok || seen[arn] {
				continue
			}
			seen[arn] = true
			out = append(out, Profile{ARN: arn, Name: strings.TrimSpace(p.Name), Region: reg})
		}
		if next = strings.TrimSpace(r.NextToken); next == "" {
			break
		}
	}
	return out, nil
}

// cliRuntimeURL API Key 账号的推理入口（Kiro CLI runtime）。
func cliRuntimeURL(cred Cred) string {
	r := strings.ToLower(strings.TrimSpace(cred.Region))
	if !regionPattern.MatchString(r) {
		r = defaultRegion
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", r)
}
