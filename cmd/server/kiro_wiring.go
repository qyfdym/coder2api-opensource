package main

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxycore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// buildKiro 按 config.kiro 构建 Kiro 账号运维服务；kiro.enabled=false 时返回 nil。
func buildKiro(cfg *Config) *kiroacct.Service {
	if !cfg.Kiro.Enabled {
		return nil
	}
	client := kiro.NewClient(kiro.Options{
		HeaderTimeout: time.Duration(cfg.Kiro.HeaderTimeoutSeconds) * time.Second,
		ProxyURL:      cfg.Kiro.ProxyURL,
		ProxyPool:     cfg.Kiro.ProxyPool,
		Endpoint:      cfg.Kiro.Endpoint,
		KiroVersion:   cfg.Kiro.KiroVersion,
	})
	// 面板托管的代理池：与 state.json 同目录，启动时载入、热生效。
	proxies, err := kiroacct.OpenProxyStore(stateSibling(cfg.StateFile, "kiro-proxies.json"))
	if err != nil {
		log.Printf("WARN: [kiro] 代理池: %v", err)
	}
	client.SetManagedProxies(proxies.EnabledURLs())
	if n := len(proxies.EnabledURLs()); n > 0 {
		log.Printf("[kiro] 面板代理池: %d 个已启用", n)
	}
	svc := &kiroacct.Service{Client: client, AuthDir: cfg.AuthDir, Proxies: proxies}
	// WorkBuddy / Qoder 的出站代理开关：面板里打开后走这个代理池，默认关闭（跟随环境变量）。
	egress.Set(svc.EgressResolver())
	return svc
}

// buildProxyCore 机场订阅与代理内核（与 state.json 同目录的 proxycore/）。依赖 Kiro：代理池在 Kiro 客户端里。
// 勾选的订阅节点作为本机代理口加入代理池；内核状态（是否运行、订阅、勾选）都在面板里管理。
func buildProxyCore(cfg *Config, kr *kiroacct.Service) *proxycore.Manager {
	if kr == nil {
		return nil
	}
	var core *proxycore.Manager
	core, err := proxycore.New(proxycore.Options{
		Dir: stateSibling(cfg.StateFile, "proxycore"),
		// 直连拉订阅失败时（很多机场不让国内 IP 直接拉）经代理池重试：订阅节点优先，连不上的跳过。
		FetchVia: func() []string {
			var nodes, others []string
			for _, m := range kr.Client.PoolMembers() {
				if m.Down {
					continue
				}
				if core != nil && core.IsNodeURL(m.URL) {
					nodes = append(nodes, m.URL)
				} else {
					others = append(others, m.URL)
				}
			}
			return append(nodes, others...)
		},
		// 节点集合变了（启停内核、勾选变化）：同步进代理池，并马上探测一遍——内核刚起来时，
		// 之前「连不上」的标记已经过时，不清掉的话账号要等一分钟才回到原来的节点。
		OnChange: func() {
			kr.SyncManagedProxies()
			go kr.Client.ProbePool(context.Background())
		},
	})
	if err != nil {
		log.Printf("WARN: [proxycore] %v", err)
		if core == nil {
			return nil
		}
	}
	kr.NodeURLs = core.NodeURLs
	kr.SyncManagedProxies()
	if n := len(core.NodeURLs()); n > 0 {
		log.Printf("[proxycore] %d 个订阅节点在代理池里", n)
	}
	return core
}

// kiroOps 把 kiroacct.Service 适配成调度器的 KiroOps（调度器不直接依赖 Kiro 协议包）。
type kiroOps struct{ svc *kiroacct.Service }

var _ scheduler.KiroOps = kiroOps{}

const kiroOpTimeout = time.Minute

// Keepalive 强制刷新一次 token（保持 refresh token 常新；API Key 账号是空操作）。
func (o kiroOps) Keepalive(a *auth.Auth) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kiroOpTimeout)
	defer cancel()
	if kerr := o.svc.Refresh(ctx, a); kerr != nil {
		// 必须返回字面 nil 之外的 error：*kiro.Error 的 nil 指针装进 error 接口会变成非 nil。
		return kerr.Kind == kiro.KindSessionDead, kerr
	}
	return false, nil
}

// Balance 查询额度（池内整数口径）+ 下次重置时刻。
func (o kiroOps) Balance(a *auth.Auth) (int64, int64, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kiroOpTimeout)
	defer cancel()
	remain, total, _, kerr := o.svc.Balance(ctx, a)
	if kerr != nil {
		return 0, 0, time.Time{}, kerr
	}
	return remain, total, kiroacct.NextReset(a, time.Now()), nil
}

var _ scheduler.KiroExhaustPolicy = kiroOps{}

// ShouldCoolOnExhausted 额度用完是否冷却：已打开超额的号不冷却（上游仍放行，超出部分计费）。
func (o kiroOps) ShouldCoolOnExhausted(a *auth.Auth, remain, total int64) bool {
	return kiroacct.ShouldCoolOnExhausted(a, remain, total)
}

// schedulerKiroOps nil 服务 → nil 接口（调度器据此整体跳过 Kiro 分支）。
func schedulerKiroOps(svc *kiroacct.Service) scheduler.KiroOps {
	if svc == nil {
		return nil
	}
	return kiroOps{svc: svc}
}

// warmKiro 启动预热：为可用账号（最多 8 个）各拉一次动态模型目录并成全池目录（/v1/models、积分倍率、
// 跨平台回退的等价模型都读这份缓存），并逐号刷新一次余额（面板首屏与积分保底立即可用）。
// 失败只记 WARN：目录有内置列表兜底，余额由周期刷新补上。
func warmKiro(ctx context.Context, svc *kiroacct.Service, p *pool.Pool) {
	if svc == nil {
		return
	}
	var accts []*auth.Auth
	for _, uid := range p.AvailableUIDsForRealm(auth.RealmKiro) {
		if a := p.AuthByUID(uid); a != nil && a.IsKiro() {
			accts = append(accts, a)
		}
	}
	if len(accts) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		for _, e := range svc.RefreshCatalogs(cctx, accts, kiroacct.CatalogOpts{Max: 8, EnsureFresh: true}) {
			log.Printf("WARN: [kiro] warm: models: %s", e)
		}
		cancel()
		log.Printf("[kiro] warm: %d models", len(svc.Client.CachedModels()))
	}
	for _, st := range p.List() {
		if ctx.Err() != nil {
			return
		}
		a := p.AuthByUID(st.UID)
		if a == nil || !a.IsKiro() || st.Disabled {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		remain, total, _, kerr := svc.Balance(cctx, a)
		cancel()
		if kerr != nil {
			log.Printf("WARN: [kiro] warm balance %s: %v", logfmt.Label(a.UID, a.Nickname), kerr)
			continue
		}
		p.ReenableIfCredits(a.UID, remain, total)
		p.SetCreditsSnapshot(a.UID, pool.CreditsSnapshot{Remain: remain, Total: total})
	}
}
