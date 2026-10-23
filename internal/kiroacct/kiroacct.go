// Package kiroacct Kiro 账号运维：网关账号（auth.Auth）与 Kiro 协议客户端（internal/kiro）之间的
// 适配层——凭证快照、刷新（账号级串行 + 落盘）、额度查询、建档（登录 / 导入）。
//
// 与 internal/qoderacct 同构：协议细节在 internal/kiro，账号池语义在 internal/pool，这里只做
// 「拿账号调协议、把结果写回账号」。
package kiroacct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Service Kiro 账号运维入口（进程内单例）。
type Service struct {
	Client *kiro.Client
	// AuthDir 账号凭证目录（与其它平台同目录，文件名 kiro-<uid>.json）。
	AuthDir string
	// LocalCacheDir 读取本机 Kiro IDE 登录缓存的目录；空 = ~/.aws/sso/cache。仅测试覆盖用。
	LocalCacheDir string

	// ChatFunc 推理入口覆盖（测试注入假上游用）；nil = Client.ChatStream。
	ChatFunc func(ctx context.Context, cred kiro.Cred, body []byte) (io.ReadCloser, *kiro.Error)

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// profileTried uid → 上次尝试补 profileArn 的时刻（失败后 30 分钟内不再每次推理都查）。
	profileTried map[string]time.Time
}

// Cred 从账号构造 Kiro 凭证快照（token 字段经锁读取）。
func Cred(a *auth.Auth) kiro.Cred {
	m := a.KiroMeta()
	return kiro.Cred{
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		ExpiresAt:    a.ExpiresAtValue(),
		AuthMethod:   m.AuthMethod,
		Provider:     m.Provider,
		Region:       m.Region,
		ClientID:     m.ClientID,
		ClientSecret: m.ClientSecret,
		StartURL:     m.StartURL,
		ProfileArn:   m.ProfileArn,
		MachineID:    m.MachineID,
		APIKey:       m.APIKey,

		TokenEndpoint: m.TokenEndpoint,
		IssuerURL:     m.IssuerURL,
		Scopes:        m.Scopes,
	}
}

// OverageEnabled 账号是否打开了超额（额度用完后按量计费）。打开时额度用完不应冷却：上游仍会放行。
func OverageEnabled(a *auth.Auth) bool {
	return strings.EqualFold(a.KiroMeta().OverageStatus, "ENABLED")
}

// ShouldCoolOnExhausted 额度刷新后是否应把账号硬冷却到下次重置：额度用完（total>0 才算「有额度的号用完了」，
// 查不到额度不算）且没有打开超额。三个刷新入口（调度器 / 面板额度 / 面板积分明细）共用，口径一致。
func ShouldCoolOnExhausted(a *auth.Auth, remain, total int64) bool {
	return remain <= 0 && total > 0 && !OverageEnabled(a)
}

// IsAPIKey 账号是否为 Kiro API Key（不刷新、不过期）。
func IsAPIKey(a *auth.Auth) bool { return Cred(a).IsAPIKey() }

func (s *Service) lockUID(uid string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m := s.locks[uid]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[uid] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ChatStream 用账号发起一次推理（输出恒为 OpenAI SSE，流尾带 usage 帧）。
func (s *Service) ChatStream(ctx context.Context, a *auth.Auth, body []byte) (io.ReadCloser, *kiro.Error) {
	s.ensureProfile(ctx, a)
	if s.ChatFunc != nil {
		return s.ChatFunc(ctx, Cred(a), body)
	}
	return s.Client.ChatStream(ctx, Cred(a), body)
}

// ensureProfile 缺 profileArn 的账号补一个并落盘（尽力而为，失败不影响本次推理）：
// Builder ID / 社交登录用固定值，企业账号查 ListAvailableProfiles。
func (s *Service) ensureProfile(ctx context.Context, a *auth.Auth) {
	m := a.KiroMeta()
	if m.ProfileArn != "" || IsAPIKey(a) || s.Client == nil {
		return
	}
	s.mu.Lock()
	if s.profileTried == nil {
		s.profileTried = map[string]time.Time{}
	}
	if t, ok := s.profileTried[a.UID]; ok && time.Since(t) < 30*time.Minute {
		s.mu.Unlock()
		return
	}
	s.profileTried[a.UID] = time.Now()
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	arn, err := s.Client.ResolveProfileArn(cctx, Cred(a))
	if err != nil || arn == "" {
		return
	}
	a.UpdateKiro(func(m *auth.KiroMeta) { m.ProfileArn = arn })
	if serr := a.SaveAtomic(); serr != nil {
		log.Printf("WARN: [kiro] save profileArn %s: %v", logfmt.Label(a.UID, a.Nickname), serr)
	}
}

// Refresh 刷新 token 并写回账号、落盘。API Key 账号是空操作。
func (s *Service) Refresh(ctx context.Context, a *auth.Auth) *kiro.Error {
	if IsAPIKey(a) {
		return nil
	}
	unlock := s.lockUID(a.UID)
	defer unlock()
	return s.refreshLocked(ctx, a)
}

func (s *Service) refreshLocked(ctx context.Context, a *auth.Auth) *kiro.Error {
	ts, err := s.Client.Refresh(ctx, Cred(a))
	if err != nil {
		return err
	}
	a.SetTokens(ts.AccessToken, ts.RefreshToken, ts.ExpiresAt)
	if ts.ProfileArn != "" {
		a.UpdateKiro(func(m *auth.KiroMeta) { m.ProfileArn = ts.ProfileArn })
	}
	if serr := a.SaveAtomic(); serr != nil {
		// refresh token 可能已轮换：不落盘的话重启后账号就救不回来了，必须大声报出来。
		log.Printf("ERR: [kiro] refresh %s: save auth failed: %v", logfmt.Label(a.UID, a.Nickname), serr)
	}
	return nil
}

// EnsureFresh access token 将在 skew 内过期时先刷新（API Key 账号永不过期）。
func (s *Service) EnsureFresh(ctx context.Context, a *auth.Auth, skew time.Duration) *kiro.Error {
	if IsAPIKey(a) || !a.NeedsRefresh(skew) {
		return nil
	}
	unlock := s.lockUID(a.UID)
	defer unlock()
	if !a.NeedsRefresh(skew) {
		return nil // 等锁期间别的路径已经刷新完
	}
	return s.refreshLocked(ctx, a)
}

// NeedsRefresh 账号是否需要先刷新再用（API Key 账号恒为 false）。
func NeedsRefresh(a *auth.Auth, skew time.Duration) bool {
	return !IsAPIKey(a) && a.NeedsRefresh(skew)
}

const opSkew = 5 * time.Minute

// Balance 查询额度并补全账号资料（邮箱 / 套餐 / 下次重置时刻），返回账号池的整数口径。
func (s *Service) Balance(ctx context.Context, a *auth.Auth) (remain, total int64, u kiro.Usage, kerr *kiro.Error) {
	if kerr = s.EnsureFresh(ctx, a, opSkew); kerr != nil {
		return 0, 0, u, kerr
	}
	s.ensureProfile(ctx, a)
	u, kerr = s.Client.UsageLimits(ctx, Cred(a))
	if kerr != nil {
		return 0, 0, u, kerr
	}
	r, t := u.Totals(time.Now())
	remain, total = int64(math.Floor(r)), int64(math.Round(t))
	if total < remain {
		total = remain
	}
	s.recordUsage(a, u)
	return remain, total, u, nil
}

// recordUsage 把额度接口顺带返回的资料（套餐 / 邮箱 / 重置时刻 / 超额状态）写回账号并落盘。
func (s *Service) recordUsage(a *auth.Auth, u kiro.Usage) {
	changed := false
	a.UpdateKiro(func(m *auth.KiroMeta) {
		set := func(dst *string, v string) {
			if v != "" && *dst != v {
				*dst, changed = v, true
			}
		}
		set(&m.Plan, u.Plan)
		set(&m.PlanTitle, u.PlanTitle)
		set(&m.Email, u.Email)
		set(&m.UserID, u.UserID)
		set(&m.OverageStatus, u.OverageStatus)
		if u.NextResetAt > 0 && m.NextResetAt != u.NextResetAt {
			m.NextResetAt, changed = u.NextResetAt, true
		}
	})
	if a.Nickname == "" && u.Email != "" {
		a.Lock()
		a.Nickname = u.Email
		a.Unlock()
		changed = true
	}
	if changed {
		_ = a.SaveAtomic()
	}
}

// NextReset 额度耗尽时的冷却截止：账号记录的下次重置时刻；未知时 6 小时后再看。
func NextReset(a *auth.Auth, now time.Time) time.Time {
	if r := a.KiroMeta().NextResetAt; r > now.Unix() {
		return time.Unix(r, 0)
	}
	return now.Add(6 * time.Hour)
}

// ---------------------------------------------------------------------------
// 建档
// ---------------------------------------------------------------------------

// NewAccountFromLogin 设备授权成功后建档。
func (s *Service) NewAccountFromLogin(ctx context.Context, sess *kiro.LoginSession, ts kiro.TokenSet) (*auth.Auth, error) {
	source := "builder_id"
	if sess.Provider != "BuilderId" {
		source = "idc"
	}
	return s.createAccount(ctx, kiro.ImportItem{
		AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken, ExpiresAt: ts.ExpiresAt,
		ClientID: sess.ClientID, ClientSecret: sess.ClientSecret, AuthMethod: "idc",
		Provider: sess.Provider, Region: sess.Region, StartURL: sess.StartURL,
	}, source, false)
}

// ImportResult 一条导入结果。
type ImportResult struct {
	OK       bool   `json:"ok"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Error    string `json:"error,omitempty"`
	Account  *auth.Auth
}

// Import 逐条导入（OAuth 凭证先刷新一次确认可用；API Key 先查一次额度确认有效）。
func (s *Service) Import(ctx context.Context, items []kiro.ImportItem) []ImportResult {
	return s.importWith(ctx, items, "import", true)
}

// importWith 导入的通用实现。forceRefresh=false 时 access token 还有效就不刷新（不轮换 refresh token）。
func (s *Service) importWith(ctx context.Context, items []kiro.ImportItem, source string, forceRefresh bool) []ImportResult {
	out := make([]ImportResult, 0, len(items))
	for _, it := range items {
		a, err := s.createAccount(ctx, it, source, forceRefresh)
		if err != nil {
			out = append(out, ImportResult{Error: err.Error(), Nickname: firstNonEmpty(it.Nickname, it.Email)})
			continue
		}
		out = append(out, ImportResult{OK: true, UID: a.UID, Nickname: a.Nickname, Account: a})
	}
	return out
}

// ImportSSOTokens 逐个用 SSO token（x-amz-sso_authn）换出 Builder ID 凭证并建档。
// 错误信息里只带序号，不带 token 本身。
func (s *Service) ImportSSOTokens(ctx context.Context, tokens []string, region string) []ImportResult {
	out := make([]ImportResult, 0, len(tokens))
	for i, tok := range tokens {
		label := fmt.Sprintf("SSO token #%d", i+1)
		r, kerr := s.Client.ImportFromSSOToken(ctx, tok, region)
		if kerr != nil {
			out = append(out, ImportResult{Nickname: label, Error: kerr.Error()})
			continue
		}
		a, err := s.createAccount(ctx, kiro.ImportItem{
			AccessToken: r.Tokens.AccessToken, RefreshToken: r.Tokens.RefreshToken, ExpiresAt: r.Tokens.ExpiresAt,
			ClientID: r.ClientID, ClientSecret: r.ClientSecret, AuthMethod: "idc", Provider: "BuilderId",
			Region: r.Region, StartURL: kiro.BuilderIDStartURL,
		}, "sso_token", false)
		if err != nil {
			out = append(out, ImportResult{Nickname: label, Error: err.Error()})
			continue
		}
		out = append(out, ImportResult{OK: true, UID: a.UID, Nickname: a.Nickname, Account: a})
	}
	return out
}

// LocalCacheDirPath 本机 Kiro IDE 缓存目录（未配置时取 ~/.aws/sso/cache）。
func (s *Service) LocalCacheDirPath() (string, error) {
	if d := strings.TrimSpace(s.LocalCacheDir); d != "" {
		return d, nil
	}
	return kiro.DefaultLocalCacheDir()
}

// ImportLocalCache 导入网关所在机器上的 Kiro IDE 登录缓存。access token 还有效就不刷新，
// 尽量推迟 refresh token 轮换（轮换会让 IDE 手里的那份失效）。
func (s *Service) ImportLocalCache(ctx context.Context) ([]ImportResult, []string) {
	dir, err := s.LocalCacheDirPath()
	if err != nil {
		return nil, []string{err.Error()}
	}
	items, skipped := kiro.ReadLocalCache(dir)
	if len(items) == 0 {
		return nil, skipped
	}
	return s.importWith(ctx, items, "local_cache", false), skipped
}

// PortalProfiles 网页登录（企业 SSO）完成后列出该凭证能用的 profile；多于一个时由用户挑选。
func (s *Service) PortalProfiles(ctx context.Context, res *kiro.PortalResult) ([]kiro.Profile, *kiro.Error) {
	if res == nil || res.Kind != kiro.AuthMethodExternalIDP {
		return nil, nil
	}
	return s.Client.ListProfiles(ctx, kiro.Cred{
		AccessToken: res.Tokens.AccessToken, AuthMethod: kiro.AuthMethodExternalIDP, Provider: kiro.ProviderAzureAD,
		ClientID: res.ClientID, TokenEndpoint: res.TokenEndpoint, IssuerURL: res.IssuerURL, Scopes: res.Scopes,
	})
}

// NewAccountFromPortal 网页登录成功后建档。profileArn 非空时用它（企业 SSO 多 profile 时用户的选择）。
func (s *Service) NewAccountFromPortal(ctx context.Context, res *kiro.PortalResult, profileArn string) (*auth.Auth, error) {
	if res == nil {
		return nil, errors.New("登录结果为空")
	}
	it := kiro.ImportItem{
		AccessToken: res.Tokens.AccessToken, RefreshToken: res.Tokens.RefreshToken, ExpiresAt: res.Tokens.ExpiresAt,
		ProfileArn: firstNonEmpty(profileArn, res.Tokens.ProfileArn), Region: "us-east-1",
	}
	source := "portal_social"
	if res.Kind == kiro.AuthMethodExternalIDP {
		it.AuthMethod, it.Provider = kiro.AuthMethodExternalIDP, kiro.ProviderAzureAD
		it.ClientID, it.TokenEndpoint, it.IssuerURL, it.Scopes = res.ClientID, res.TokenEndpoint, res.IssuerURL, res.Scopes
		it.Email, it.UserID = res.Email, res.UserID
		source = "portal_microsoft"
	} else {
		it.AuthMethod, it.Provider = "social", "Social"
	}
	return s.createAccount(ctx, it, source, false)
}

// OverageInfo 账号的超额（额度用完后按量计费）状态。
type OverageInfo struct {
	Status          string  // ENABLED / DISABLED / 空 = 上游没给
	Capable         bool    // 订阅是否支持超额
	Capability      string  // 上游原值（OVERAGE_CAPABLE / NOT_OVERAGE_CAPABLE …）
	CapUSD          float64 // 超额费用上限（美元）
	RateUSD         float64 // 单次调用超额单价（美元）
	CurrentUSD      float64 // 本周期已产生的超额费用（美元）
	SubscriptionTit string
}

// overageCapable 订阅是否支持超额。空串 = 上游没给，视为未知并放行；否则必须是明确的
// 「CAPABLE」且不含任何否定写法（INCAPABLE / NOT_… / UNSUPPORTED / DISABLED）。
func overageCapable(capability string) bool {
	c := strings.ToUpper(strings.TrimSpace(capability))
	if c == "" {
		return true
	}
	for _, neg := range []string{"INCAPABLE", "NOT", "UNSUPPORTED", "UNAVAILABLE", "DISABLED"} {
		if strings.Contains(c, neg) {
			return false
		}
	}
	return strings.Contains(c, "CAPABLE")
}

func overageInfo(u kiro.Usage) OverageInfo {
	capb := strings.ToUpper(u.OverageCapability)
	return OverageInfo{
		Status: u.OverageStatus, Capability: u.OverageCapability,
		// 字段缺失视为未知，放行让上游自己决定（不支持会拒绝设置）；有值就只认明确的肯定值。
		// 真机里学生号返回的是 OVERAGE_INCAPABLE——「INCAPABLE」里没有 NOT，只检查否定词会判错。
		Capable: overageCapable(capb),
		CapUSD:  u.OverageCap, RateUSD: u.OverageRate, CurrentUSD: u.CurrentOverages, SubscriptionTit: u.PlanTitle,
	}
}

// Overage 读取账号当前的超额开关状态（顺带刷新账号资料）。
func (s *Service) Overage(ctx context.Context, a *auth.Auth) (OverageInfo, *kiro.Error) {
	if kerr := s.EnsureFresh(ctx, a, opSkew); kerr != nil {
		return OverageInfo{}, kerr
	}
	u, kerr := s.Client.UsageLimits(ctx, Cred(a))
	if kerr != nil {
		return OverageInfo{}, kerr
	}
	s.recordUsage(a, u)
	return overageInfo(u), nil
}

// SetOverage 打开 / 关闭超额开关，并把新状态写回账号。
func (s *Service) SetOverage(ctx context.Context, a *auth.Auth, enabled bool) (OverageInfo, *kiro.Error) {
	if kerr := s.EnsureFresh(ctx, a, opSkew); kerr != nil {
		return OverageInfo{}, kerr
	}
	s.ensureProfile(ctx, a)
	u, kerr := s.Client.SetOverage(ctx, Cred(a), enabled)
	if kerr != nil {
		return OverageInfo{}, kerr
	}
	s.recordUsage(a, u)
	return overageInfo(u), nil
}

// createAccount 由凭证建档：OAuth 凭证（forceRefresh 或 access token 将过期时）先刷新，
// 补 profileArn，再查一次额度拿邮箱 / userId（也顺带验证凭证），最后落盘。
func (s *Service) createAccount(ctx context.Context, it kiro.ImportItem, source string, forceRefresh bool) (*auth.Auth, error) {
	if strings.TrimSpace(s.AuthDir) == "" {
		return nil, errors.New("auth_dir 未配置")
	}
	cred := kiro.Cred{
		AccessToken: it.AccessToken, RefreshToken: it.RefreshToken, ExpiresAt: it.ExpiresAt,
		AuthMethod: it.AuthMethod, Provider: it.Provider, Region: it.Region,
		ClientID: it.ClientID, ClientSecret: it.ClientSecret, StartURL: it.StartURL,
		ProfileArn: it.ProfileArn, APIKey: it.APIKey,
		TokenEndpoint: it.TokenEndpoint, IssuerURL: it.IssuerURL, Scopes: it.Scopes,
	}
	if cred.IsExternalIDP() {
		// 企业 SSO 是公共客户端：不存 clientSecret；端点 / scope 入库前再校验一遍。
		cred.ClientSecret = ""
		if err := kiro.ValidateExternalIdpConfig(cred.ClientID, cred.TokenEndpoint, cred.IssuerURL, cred.Scopes); err != nil {
			return nil, fmt.Errorf("企业 SSO 配置无效：%v", err)
		}
	}
	if cred.IsAPIKey() {
		cred.AccessToken, cred.RefreshToken, cred.ExpiresAt = it.APIKey, "", 0
		cred.MachineID = kiro.MachineIDFromAPIKey(it.APIKey)
		source = "api_key"
	} else {
		cred.MachineID = kiro.NewMachineID()
		if forceRefresh || cred.AccessToken == "" || cred.ExpiresAt <= time.Now().Add(opSkew).Unix() {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ts, kerr := s.Client.Refresh(rctx, cred)
			cancel()
			if kerr != nil {
				return nil, fmt.Errorf("刷新 token 失败：%v", kerr)
			}
			cred.AccessToken, cred.RefreshToken, cred.ExpiresAt = ts.AccessToken, ts.RefreshToken, ts.ExpiresAt
			if ts.ProfileArn != "" && cred.ProfileArn == "" {
				cred.ProfileArn = ts.ProfileArn
			}
		}
		if cred.IsExternalIDP() && cred.ProfileArn != "" {
			// 企业 SSO 的 profileArn 只接受该凭证自己能列出来的，不让外部输入把任意数据面 ARN 钉到一个可用凭证上。
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			profiles, kerr := s.Client.ListProfiles(pctx, cred)
			cancel()
			if kerr != nil {
				return nil, fmt.Errorf("无法核对 profileArn：%v", kerr)
			}
			found := false
			for _, p := range profiles {
				found = found || p.ARN == cred.ProfileArn
			}
			if !found {
				return nil, errors.New("profileArn 不在该账号可用的 profile 列表里")
			}
		}
		if cred.ProfileArn == "" {
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if arn, kerr := s.Client.ResolveProfileArn(pctx, cred); kerr == nil && arn != "" {
				cred.ProfileArn = arn
			}
			cancel()
		}
	}
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	usage, uerr := s.Client.UsageLimits(uctx, cred)
	cancel()
	if uerr != nil {
		switch uerr.Kind {
		case kiro.KindAuth, kiro.KindSessionDead, kiro.KindSuspended:
			return nil, fmt.Errorf("凭证校验失败：%v", uerr)
		}
		// 网络抖动 / 服务端错误不阻止建档（额度稍后由后台刷新补上）。
		log.Printf("WARN: [kiro] new account: usage check failed (account saved anyway): %v", uerr)
	}
	email := firstNonEmpty(usage.Email, it.Email)
	if email == "" && cred.IsExternalIDP() {
		email, _ = kiro.ExternalIdpIdentity(cred.AccessToken)
	}
	identity := firstNonEmpty(usage.UserID, it.UserID, email)
	switch {
	case identity != "":
	case cred.IsAPIKey():
		identity = "apikey:" + it.APIKey
	default:
		identity = "rt:" + it.RefreshToken
	}
	uid := kiro.StableUID(identity)
	nick := firstNonEmpty(it.Nickname, email)
	if nick == "" {
		nick = "kiro-" + uid[1:7]
	}
	meta := auth.KiroMeta{
		AuthMethod: strings.ToLower(cred.AuthMethod), Provider: cred.Provider, Region: cred.Region,
		ClientID: cred.ClientID, ClientSecret: cred.ClientSecret, StartURL: cred.StartURL,
		ProfileArn: cred.ProfileArn, MachineID: cred.MachineID, APIKey: cred.APIKey,
		TokenEndpoint: cred.TokenEndpoint, IssuerURL: cred.IssuerURL, Scopes: cred.Scopes,
		Email: email, UserID: firstNonEmpty(usage.UserID, it.UserID), Plan: usage.Plan, PlanTitle: usage.PlanTitle,
		NextResetAt: usage.NextResetAt, OverageStatus: usage.OverageStatus, Source: source, AddedAt: time.Now().Unix(),
	}
	if meta.AuthMethod == "" {
		meta.AuthMethod = "social"
	}
	a := auth.NewKiro(uid, nick, cred.AccessToken, cred.RefreshToken, cred.ExpiresAt, meta)
	a.FilePath = filepath.Join(s.AuthDir, auth.KiroFileName(uid))
	if err := a.SaveAtomic(); err != nil {
		return nil, err
	}
	return a, nil
}

// CatalogOpts RefreshCatalogs 的选项。
type CatalogOpts struct {
	// Max 单次最多拉几个账号的目录（0 = 4）；目录缺失的账号优先，其余的下次再补。
	Max int
	// Force 忽略缓存有效期，强制重拉。
	Force bool
	// EnsureFresh 账号 token 将过期时先刷新；false 时直接跳过这类账号（/v1/models 路径不为刷新 token 拖慢响应）。
	EnsureFresh bool
}

// RefreshCatalogs 为目录缺失 / 过期的账号各拉一次模型目录，并入全池目录（见 kiro 包 modelCache）。
// 目录按账号授权返回，只拉一个账号会漏掉其它账号独有的模型。返回各账号的失败原因（成功的不返回）。
func (s *Service) RefreshCatalogs(ctx context.Context, accts []*auth.Auth, opt CatalogOpts) []string {
	if s == nil || s.Client == nil {
		return nil
	}
	limit := opt.Max
	if limit <= 0 {
		limit = 4
	}
	var missing, stale []*auth.Auth
	for _, a := range accts {
		if a == nil || !a.IsKiro() {
			continue
		}
		switch {
		case !s.Client.CatalogFresh(a.UID) && !s.Client.HasCatalog(a.UID):
			missing = append(missing, a)
		case opt.Force || !s.Client.CatalogFresh(a.UID):
			stale = append(stale, a)
		}
	}
	var errs []string
	for _, a := range append(missing, stale...) {
		if limit == 0 || ctx.Err() != nil {
			break
		}
		if NeedsRefresh(a, time.Minute) {
			if !opt.EnsureFresh {
				continue
			}
			if kerr := s.EnsureFresh(ctx, a, 5*time.Minute); kerr != nil {
				errs = append(errs, logfmt.Label(a.UID, a.Nickname)+": "+kerr.Error())
				continue
			}
		}
		limit--
		if _, kerr := s.Client.RefreshModels(ctx, Cred(a)); kerr != nil {
			errs = append(errs, logfmt.Label(a.UID, a.Nickname)+": "+kerr.Error())
		}
	}
	return errs
}

// ModelRate 模型 → 积分倍率字符串（"1.3"；未知为空）。供账号池积分保底判收费。
func (s *Service) ModelRate(model string) string {
	if s == nil || s.Client == nil {
		return ""
	}
	m, ok := s.Client.LookupModel(model)
	if !ok || m.RateMultiplier <= 0 {
		return ""
	}
	return strconv.FormatFloat(m.RateMultiplier, 'f', -1, 64)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
