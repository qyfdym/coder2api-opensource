// Package qoderacct 把 Qoder 协议实现（internal/qoder）接到网关的统一账号体系上。
//
// 分工：
//   - internal/qoder 只懂「给一个凭证，怎么和 Qoder 服务端通信」，不碰持久化、不碰账号池；
//   - 本包负责 auth.Auth ↔ qoder.Cred 的转换、刷新后的凭证写回与落盘、签到留痕（兑换码 /
//     同人冷却 / 最近签到）的持久化，以及新账号（OAuth / PAT）建档；
//   - server / scheduler / panel 只调本包，不直接拼 Qoder 凭证。
//
// 这样 Qoder 的账号运维语义只有一份实现，三个调用方不会各写一套「刷新完记得存盘」。
package qoderacct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
)

// Service Qoder 账号运维入口（进程内单例）。
type Service struct {
	Client *qoder.Client
	// AuthDir 账号凭证目录（与 WorkBuddy 账号同目录，文件名 qoder-<uid>.json）。
	AuthDir string
	// DefaultRealm qoder: 前缀下两区都有同名模型时优先使用的区域（cn / intl）。
	DefaultRealm string

	// ChatFunc 推理入口覆盖（测试注入假上游用）；nil = Client.ChatStream。
	ChatFunc func(ctx context.Context, cred qoder.Cred, body []byte) (io.ReadCloser, *qoder.Error)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // 账号级刷新锁（见 lockUID）
}

// ChatStream 用账号凭证发起一次推理（输出恒为 OpenAI SSE，流尾带 usage 帧）。
func (s *Service) ChatStream(ctx context.Context, a *auth.Auth, body []byte) (io.ReadCloser, *qoder.Error) {
	if s.ChatFunc != nil {
		return s.ChatFunc(ctx, Cred(a), body)
	}
	return s.Client.ChatStream(ctx, Cred(a), body)
}

// Cred 从账号构造 Qoder 凭证快照（token 字段经锁读取）。
func Cred(a *auth.Auth) qoder.Cred {
	m := a.QoderMeta()
	return qoder.Cred{
		UID:          a.UID,
		Nickname:     a.Nickname,
		Realm:        a.QoderRegion(),
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		PAT:          m.PAT,
		ExpiresAt:    a.ExpiresAtValue(),
		UserType:     m.UserType,
		OrgID:        m.OrgID,
		OrgName:      m.OrgName,
	}
}

// Region 路由域 → Qoder 区域（qoder-intl → intl，其余 → cn）。
func Region(realm string) string {
	if realm == auth.RealmQoderIntl {
		return qoder.RealmIntl
	}
	return qoder.RealmCN
}

// RouteRealm Qoder 区域 → 路由域。
func RouteRealm(region string) string {
	if qoder.NormalizeRealm(region) == qoder.RealmIntl {
		return auth.RealmQoderIntl
	}
	return auth.RealmQoderCN
}

// lockUID 取账号级刷新锁。
//
// 必须串行：drt- refresh token 每用一次就换新、旧的立即作废。推理、签到、余额、保活四条
// 路径可能同时发现 token 将过期，若并发刷新，后到的那个会拿已作废的旧 refresh token
// 去换，上游回会话失效——账号就被自己打死了。
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

// Refresh 刷新 token 并写回账号、落盘、作废旧的 COSY 会话。
// 返回的错误保留 Qoder 分类（KindSessionDead = 需要重新登录），调用方据此停用 / 冷却。
func (s *Service) Refresh(ctx context.Context, a *auth.Auth) *qoder.Error {
	unlock := s.lockUID(a.UID)
	defer unlock()
	return s.refreshLocked(ctx, a)
}

func (s *Service) refreshLocked(ctx context.Context, a *auth.Auth) *qoder.Error {
	ts, err := s.Client.Refresh(ctx, Cred(a))
	if err != nil {
		return err
	}
	a.SetTokens(ts.AccessToken, ts.RefreshToken, ts.ExpiresAt)
	s.Client.Sessions.Invalidate(a.UID)
	if serr := a.SaveAtomic(); serr != nil {
		// 刷新成功但落盘失败：drt- 每次刷新都会换新，旧 refresh token 已作废——
		// 不落盘的话重启后账号就救不回来了，必须大声报出来。
		log.Printf("ERR: [qoder] refresh %s: save auth failed: %v", logfmt.Label(a.UID, a.Nickname), serr)
	}
	return nil
}

// opSkew 签到 / 余额等运维请求前的刷新提前量（token 将在 5 分钟内过期就先刷新）。
const opSkew = 5 * time.Minute

// EnsureFresh access token 将在 skew 内过期时先刷新。
func (s *Service) EnsureFresh(ctx context.Context, a *auth.Auth, skew time.Duration) *qoder.Error {
	if !a.NeedsRefresh(skew) {
		return nil
	}
	unlock := s.lockUID(a.UID)
	defer unlock()
	// 等锁期间别的路径可能已经刷新完：复查一次，避免拿刚换下的旧 refresh token 再刷。
	if !a.NeedsRefresh(skew) {
		return nil
	}
	return s.refreshLocked(ctx, a)
}

// Balance 查询额度（顺带补套餐名），返回给账号池的整数口径。
//
// remain 用「有效余额」（含团队共享池）；共享池不设上限时 remain 至少记 1，
// 让账号池把它当成「有额度」（ReenableIfCredits 解冻 / 积分保底判断都只看正负）。
func (s *Service) Balance(ctx context.Context, a *auth.Auth) (remain, total int64, cr qoder.Credits, qerr *qoder.Error) {
	if qerr = s.EnsureFresh(ctx, a, opSkew); qerr != nil {
		return 0, 0, cr, qerr
	}
	cred := Cred(a)
	cr, qerr = s.Client.FetchCredits(ctx, cred)
	if qerr != nil {
		return 0, 0, cr, qerr
	}
	remain = int64(math.Floor(cr.RemainEffective))
	total = int64(math.Floor(cr.Size))
	if cr.Shared != nil && cr.Shared.Available && !cr.Shared.Unlimited && cr.Shared.Size > 0 {
		total += int64(math.Floor(cr.Shared.Size))
	}
	if cr.SharedUnlimited() && remain <= 0 {
		remain = 1
	}
	if total < remain {
		total = remain
	}
	// 套餐名只在缺失时补拉（plan 很少变化；查询失败不影响余额结果）。
	if a.QoderMeta().Plan == "" {
		if plan, perr := s.Client.FetchPlan(ctx, cred); perr == nil && plan != "" {
			a.UpdateQoder(func(m *auth.QoderMeta) { m.Plan = plan })
			_ = a.SaveAtomic()
		}
	}
	return remain, total, cr, nil
}

// CheckinMode 签到入口（对应 Qoder 官方三个按钮）。
type CheckinMode string

const (
	CheckinDaily CheckinMode = "daily" // 只领每日 Credits
	CheckinAll   CheckinMode = "all"   // 每日 Credits + 兑换码类活动 + 已达标任务奖励
	CheckinPro   CheckinMode = "pro"   // 仅领 Pro 福利包
)

// Checkin 执行签到 / 领取，并把留痕（最近签到、兑换码、同人冷却）写回账号文件。
func (s *Service) Checkin(ctx context.Context, a *auth.Auth, mode CheckinMode) (qoder.CheckinResult, *qoder.Error) {
	var (
		res  qoder.CheckinResult
		qerr *qoder.Error
	)
	if qerr = s.EnsureFresh(ctx, a, opSkew); qerr != nil {
		res.Message = "刷新凭证失败：" + qerr.Error()
		return res, qerr
	}
	cred := Cred(a)
	if mode == CheckinPro {
		res, qerr = s.Client.ClaimPro(ctx, cred)
	} else {
		opt := qoder.CheckinOptions{BlockedUntil: blockedUntil(a.QoderMeta(), time.Now())}
		res, qerr = s.Client.RunCheckin(ctx, cred, mode == CheckinDaily, opt)
	}
	now := time.Now()
	a.UpdateQoder(func(m *auth.QoderMeta) {
		if res.CheckedIn {
			m.LastCheckin = now.Format("2006-01-02 15:04:05")
		}
		if res.Message != "" {
			m.CheckinNote = res.Message
		}
		for _, c := range res.Codes {
			if c.Code == "" {
				continue
			}
			if m.CampaignCode == nil {
				m.CampaignCode = map[string]auth.QoderCampaignCode{}
			}
			m.CampaignCode[c.CampaignID] = auth.QoderCampaignCode{Campaign: c.Campaign, Code: c.Code, At: now.Unix()}
		}
		for _, b := range res.Blocked {
			if b.Cooldown || b.CampaignID == "" {
				continue
			}
			if m.CampaignBlockedUntil == nil {
				m.CampaignBlockedUntil = map[string]int64{}
			}
			m.CampaignBlockedUntil[b.CampaignID] = b.Until.Unix()
		}
		for id, until := range m.CampaignBlockedUntil {
			if until <= now.Unix() {
				delete(m.CampaignBlockedUntil, id)
			}
		}
	})
	if err := a.SaveAtomic(); err != nil {
		log.Printf("WARN: [qoder] checkin %s: save auth failed: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
	return res, qerr
}

func blockedUntil(m auth.QoderMeta, now time.Time) map[string]time.Time {
	if len(m.CampaignBlockedUntil) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(m.CampaignBlockedUntil))
	for id, u := range m.CampaignBlockedUntil {
		if t := time.Unix(u, 0); t.After(now) {
			out[id] = t
		}
	}
	return out
}

// CheckedInToday 账号今天（本地时区）是否已签到。
func CheckedInToday(a *auth.Auth, now time.Time) bool {
	lc := a.QoderMeta().LastCheckin
	return len(lc) >= 10 && lc[:10] == now.Format("2006-01-02")
}

// NewAccount 用登录 / PAT 交换得到的凭证建档并落盘（返回待入池的账号）。
func (s *Service) NewAccount(region string, ts qoder.TokenSet, u qoder.UserInfo, pat, source string) (*auth.Auth, error) {
	uid := strings.TrimSpace(u.UID)
	if uid == "" || !validUID(uid) {
		return nil, fmt.Errorf("qoder 账号 uid 非法：%q", uid)
	}
	if strings.TrimSpace(s.AuthDir) == "" {
		return nil, errors.New("auth_dir 未配置")
	}
	nick := strings.TrimSpace(u.Nickname)
	a := auth.NewQoder(uid, nick, region, ts.AccessToken, ts.RefreshToken, ts.ExpiresAt, auth.QoderMeta{
		PAT:      strings.TrimSpace(pat),
		UserType: u.UserType,
		OrgID:    u.OrgID,
		OrgName:  u.OrgName,
		Email:    u.Email,
		Source:   source,
		AddedAt:  time.Now().Unix(),
	})
	a.Domain = qoder.ConfigFor(region).Domain
	a.FilePath = filepath.Join(s.AuthDir, auth.QoderFileName(uid))
	if err := a.SaveAtomic(); err != nil {
		return nil, err
	}
	return a, nil
}

// validUID 只允许 [A-Za-z0-9_-]：uid 会拼进文件名，必须防路径穿越。
func validUID(uid string) bool {
	if uid == "" || len(uid) > 128 {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ModelRealms 模型名在哪些路由域可用（qoder-cn / qoder-intl），只读缓存与快照、不打网络。
func (s *Service) ModelRealms(model string) []string {
	var out []string
	for _, r := range s.Client.ModelRealms(model) {
		out = append(out, RouteRealm(r))
	}
	return out
}

// ModelRate 路由域 + 模型 → 积分倍率字符串（"0.5"；未知为空）。供账号池积分保底判收费。
func (s *Service) ModelRate(realm, model string) string {
	m, ok := s.Client.LookupModel(Region(realm), model)
	if !ok {
		return ""
	}
	return trimFloat(m.PriceFactor)
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.4f", f)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
