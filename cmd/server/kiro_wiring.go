package main

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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
		Endpoint:      cfg.Kiro.Endpoint,
		KiroVersion:   cfg.Kiro.KiroVersion,
	})
	return &kiroacct.Service{Client: client, AuthDir: cfg.AuthDir}
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

// schedulerKiroOps nil 服务 → nil 接口（调度器据此整体跳过 Kiro 分支）。
func schedulerKiroOps(svc *kiroacct.Service) scheduler.KiroOps {
	if svc == nil {
		return nil
	}
	return kiroOps{svc: svc}
}

// warmKiro 启动预热：用一个可用账号拉一次动态模型目录（/v1/models、积分倍率、跨平台回退的
// 等价模型都读这份缓存），并逐号刷新一次余额（面板首屏与积分保底立即可用）。
// 失败只记 WARN：目录有内置列表兜底，余额由周期刷新补上。
func warmKiro(ctx context.Context, svc *kiroacct.Service, p *pool.Pool) {
	if svc == nil {
		return
	}
	if uids := p.AvailableUIDsForRealm(auth.RealmKiro); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil && a.IsKiro() {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if kerr := svc.EnsureFresh(cctx, a, 5*time.Minute); kerr != nil {
				log.Printf("WARN: [kiro] warm: refresh %s: %v", logfmt.Label(a.UID, a.Nickname), kerr)
			} else if models, kerr := svc.Client.RefreshModels(cctx, kiroacct.Cred(a)); kerr != nil {
				log.Printf("WARN: [kiro] warm: models: %v", kerr)
			} else {
				log.Printf("[kiro] warm: %d models", len(models))
			}
			cancel()
		}
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
