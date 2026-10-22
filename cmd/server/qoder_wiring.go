package main

// qoder_wiring.go Qoder 平台的装配：客户端 + 账号运维服务 + 调度器适配 + 启动预热。

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoder"
	"github.com/linguo2625469/workbuddy2api-panel/internal/qoderacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// buildQoder 按 config.qoder 构建 Qoder 账号运维服务；qoder.enabled=false 时返回 nil。
func buildQoder(cfg *Config) *qoderacct.Service {
	if !cfg.Qoder.Enabled {
		return nil
	}
	client := qoder.NewClient(qoder.Options{
		DataDir:         cfg.QoderDataDir(),
		IntlGateway:     cfg.Qoder.IntlGateway,
		RequestTimeout:  time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
		HeaderTimeout:   time.Duration(cfg.Qoder.HeaderTimeoutSeconds) * time.Second,
		QueueMaxWait:    time.Duration(cfg.Qoder.QueueMaxWaitSeconds) * time.Second,
		RuntimeInfoPath: cfg.Qoder.RuntimeInfoPath,
		DesktopVersion:  cfg.Qoder.DesktopVersion,
	})
	return &qoderacct.Service{
		Client:       client,
		AuthDir:      cfg.AuthDir,
		DefaultRealm: cfg.Qoder.DefaultRealm,
	}
}

// qoderOps 把 qoderacct.Service 适配成调度器的 QoderOps（调度器不直接依赖 Qoder 协议包）。
type qoderOps struct{ svc *qoderacct.Service }

var _ scheduler.QoderOps = qoderOps{}

// 单号任务超时：签到含活动平台多步请求与发放后的额度复查，给足 3 分钟；其余 1 分钟。
const (
	qoderCheckinTimeout = 3 * time.Minute
	qoderOpTimeout      = time.Minute
)

// Checkin 一键签到口径（每日 Credits + 兑换码类活动 + 已达标任务奖励）。
// 上游幂等：已领过的回「今天已领过」，所以排程每个签到时点都跑一遍也不会重复发放。
func (o qoderOps) Checkin(a *auth.Auth) (bool, string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qoderCheckinTimeout)
	defer cancel()
	res, qerr := o.svc.Checkin(ctx, a, qoderacct.CheckinAll)
	if qerr != nil {
		return res.CheckedIn, res.Message, qerr.Kind == qoder.KindSessionDead, qerr
	}
	// 成功分支必须返回字面 nil：*qoder.Error 的 nil 指针装进 error 接口会变成非 nil。
	return res.CheckedIn, res.Message, false, nil
}

// Keepalive 强制刷新一次 token（drt- 每次刷新换新，保持 refresh token 常新）。
func (o qoderOps) Keepalive(a *auth.Auth) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qoderOpTimeout)
	defer cancel()
	if qerr := o.svc.Refresh(ctx, a); qerr != nil {
		return qerr.Kind == qoder.KindSessionDead, qerr
	}
	return false, nil
}

// Balance 查询额度（池内整数口径）。
func (o qoderOps) Balance(a *auth.Auth) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qoderOpTimeout)
	defer cancel()
	remain, total, _, qerr := o.svc.Balance(ctx, a)
	if qerr != nil {
		return 0, 0, qerr
	}
	return remain, total, nil
}

// schedulerQoderOps nil 服务 → nil 接口（调度器据此整体跳过 Qoder 分支）。
func schedulerQoderOps(svc *qoderacct.Service) scheduler.QoderOps {
	if svc == nil {
		return nil
	}
	return qoderOps{svc: svc}
}

// warmQoder 启动预热：每个有 Qoder 账号的区域拉一次动态模型目录（qoder:X 的区域判定、
// /v1/models、积分倍率都读这份缓存），并刷新一次余额（面板首屏与积分保底立即可用）。
// 失败只记 WARN：目录有磁盘缓存 / 内置快照兜底，余额由周期刷新补上。
func warmQoder(ctx context.Context, svc *qoderacct.Service, p *pool.Pool) {
	if svc == nil {
		return
	}
	for _, rr := range []string{auth.RealmQoderCN, auth.RealmQoderIntl} {
		if ctx.Err() != nil {
			return
		}
		uids := p.AvailableUIDsForRealm(rr)
		if len(uids) == 0 {
			continue
		}
		a := p.AuthByUID(uids[0])
		if a == nil || !a.IsQoder() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if qerr := svc.EnsureFresh(cctx, a, 5*time.Minute); qerr != nil {
			log.Printf("WARN: [qoder] warm %s: refresh: %v", rr, qerr)
		} else if models, err := svc.Client.RefreshModels(cctx, qoderacct.Region(rr), qoderacct.Cred(a)); err != nil {
			log.Printf("WARN: [qoder] warm %s: models: %v", rr, err)
		} else {
			log.Printf("[qoder] warm %s: %d models", rr, len(models))
		}
		cancel()
	}
	// 余额：池状态里可能没有（新号）或是重启前的旧值；逐号查一次，面板首屏与积分保底立即准确。
	for _, st := range p.List() {
		if ctx.Err() != nil {
			return
		}
		a := p.AuthByUID(st.UID)
		if a == nil || !a.IsQoder() || st.Disabled {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		remain, total, _, qerr := svc.Balance(cctx, a)
		cancel()
		if qerr != nil {
			log.Printf("WARN: [qoder] warm balance %s: %v", logfmt.Label(a.UID, a.Nickname), qerr)
			continue
		}
		p.ReenableIfCredits(a.UID, remain, total)
		p.SetCreditsSnapshot(a.UID, pool.CreditsSnapshot{Remain: remain, Total: total})
	}
}
