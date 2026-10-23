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
	}
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

// ensureProfile 缺 profileArn 的账号补一个（尽力而为：失败不影响推理，Builder ID 账号没有也能用）。
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
	return remain, total, u, nil
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
	out := make([]ImportResult, 0, len(items))
	for _, it := range items {
		a, err := s.createAccount(ctx, it, "import", true)
		if err != nil {
			out = append(out, ImportResult{Error: err.Error(), Nickname: firstNonEmpty(it.Nickname, it.Email)})
			continue
		}
		out = append(out, ImportResult{OK: true, UID: a.UID, Nickname: a.Nickname, Account: a})
	}
	return out
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
	identity := firstNonEmpty(usage.UserID, email)
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
		Email: email, UserID: usage.UserID, Plan: usage.Plan, PlanTitle: usage.PlanTitle,
		NextResetAt: usage.NextResetAt, Source: source, AddedAt: time.Now().Unix(),
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
