package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// acctCheckinDivergent 有意与原实现日志不一致的场景（见 tasks_run.go 文件头注释）：
// 领取请求报错时原实现打印「活动平台查询失败：None」，这里打印真实原因。
var acctCheckinDivergent = map[string]bool{"claim_http_500": true}

// TestCheckinMatchesPython 签到结论与原实现 run_checkin 对照：
// 已到账 / 今日已领 / 幂等重放 / 名额已发完 / 同人已领 / 冷却 / 无活动 / 详情类 / 混合 / 兑换码 /
// 活动平台失败后旧接口兜底（404 / 403 / 领取 / 已签 / 未开放 / 409）/ 会话死亡。
func TestCheckinMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	for _, sc := range v.Checkin {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			now := time.Now()
			rp := &acctReplay{routes: sc.Routes, subst: func(s string) string {
				return strings.ReplaceAll(s, `"__NOW__"`, strconv.FormatInt(now.Unix(), 10))
			}}
			c := acctNewClient(t, rp)
			acctWithIdentity(t, c, "tok-A") // 原实现场景里身份不影响结论；注入身份避免结论追加「未取得身份」说明
			opt := CheckinOptions{Gap: -1}
			if b := sc.Account["campaignBlockedUntil"]; b != nil {
				opt.BlockedUntil = map[string]time.Time{}
				for id := range b {
					opt.BlockedUntil[id] = now.Add(time.Hour)
				}
			}
			cred := Cred{UID: "u-test-0001", Nickname: "tester", Realm: RealmCN, AccessToken: "dt-fake", RefreshToken: "drt-fake"}
			res, err := c.RunCheckin(context.Background(), cred, sc.OnlyDaily, opt)

			if res.OK != sc.OK {
				t.Errorf("OK=%v want %v（err=%v）", res.OK, sc.OK, err)
			}
			if res.Earned != sc.Earned {
				t.Errorf("Earned=%v want %v", res.Earned, sc.Earned)
			}
			if res.CheckedIn != sc.Stamped {
				t.Errorf("CheckedIn=%v want %v", res.CheckedIn, sc.Stamped)
			}
			if sc.Message != nil { // 活动平台路径：结论文案逐字一致
				if res.Message != *sc.Message {
					t.Errorf("Message\n got %q\nwant %q", res.Message, *sc.Message)
				}
				if strings.Join(res.Claimed, "|") != strings.Join(sc.Claimed, "|") {
					t.Errorf("Claimed=%v want %v", res.Claimed, sc.Claimed)
				}
				if res.NextAvailableAt.IsZero() {
					t.Error("活动平台路径应给出 NextAvailableAt")
				}
			} else if res.Message == "" {
				t.Error("旧接口兜底路径也要给出结论文案")
			}
			if !acctCheckinDivergent[sc.Name] {
				if strings.Join(res.Logs, "\n") != strings.Join(sc.Logs, "\n") {
					t.Errorf("Logs\n got:\n  %s\nwant:\n  %s", strings.Join(res.Logs, "\n  "), strings.Join(sc.Logs, "\n  "))
				}
			}
			var gotBlocked []string
			for _, b := range res.Blocked {
				gotBlocked = append(gotBlocked, b.CampaignID)
			}
			sort.Strings(gotBlocked)
			if strings.Join(gotBlocked, ",") != strings.Join(sc.Blocked, ",") {
				t.Errorf("Blocked=%v want %v", gotBlocked, sc.Blocked)
			}
			gotCodes := map[string]string{}
			for _, cd := range res.Codes {
				gotCodes[cd.CampaignID] = cd.Code
			}
			if len(gotCodes) != len(sc.Codes) {
				t.Errorf("Codes=%v want %v", gotCodes, sc.Codes)
			}
			for k, want := range sc.Codes {
				if gotCodes[k] != want {
					t.Errorf("Codes[%s]=%q want %q", k, gotCodes[k], want)
				}
			}
			switch sc.Name {
			case "campaigns_401_session_dead":
				if err == nil || err.Kind != KindSessionDead {
					t.Errorf("会话死亡必须以 *Error 上报：%v", err)
				}
			case "claim_http_500":
				if err == nil || err.Kind != KindServer || !strings.Contains(res.Message, "活动领取失败") {
					t.Errorf("领取报错应上报并写进结论：err=%v msg=%q", err, res.Message)
				}
				for _, l := range res.Logs {
					if strings.Contains(l, "None") {
						t.Errorf("不应出现原实现的 None 文案：%s", l)
					}
				}
			case "campaigns_500_legacy_404":
				if !res.Unavailable || err == nil || err.Kind != KindServer {
					t.Errorf("活动平台 500 应以 *Error 上报，旧接口标 Unavailable：%v %+v", err, res)
				}
			case "claim_ok", "mixed_all":
				if res.Credits == nil || res.Credits.Remain != 1000 {
					t.Errorf("领取后应带回刷新的额度：%+v", res.Credits)
				}
			case "blocked_same_person":
				if b := res.Blocked[0]; b.Cooldown || time.Until(b.Until) < 5*time.Hour {
					t.Errorf("新挡下的应给 6h 冷却：%+v", b)
				}
			case "preblocked_cooldown":
				for _, call := range rp.calls {
					if strings.Contains(call, "/claim") {
						t.Errorf("冷却中不应再 POST：%v", rp.calls)
					}
				}
				if !res.Blocked[0].Cooldown {
					t.Error("冷却命中应标 Cooldown")
				}
			}
		})
	}
}

// TestCheckinRequestHeaders 活动平台走桌面端头（UA=Qoder、clienttype=10、机器头），额度走普通头。
func TestCheckinRequestHeaders(t *testing.T) {
	v := acctLoadVectors(t)
	var sc = v.Checkin[0]
	if sc.Name != "claim_ok" {
		t.Fatal("向量顺序变了")
	}
	rp := &acctReplay{routes: sc.Routes}
	c := acctNewClient(t, rp)
	acctWithIdentity(t, c, "tok-A")
	cred := Cred{UID: "u-test-0001", Nickname: "tester", AccessToken: "dt-fake"}
	if _, err := c.RunCheckin(context.Background(), cred, true, CheckinOptions{Gap: -1}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GET " + oaPathCampaigns, "POST /sash/api/v1/me/campaigns/c1/claim"} {
		h := rp.hdrs[key]
		if h.Get("User-Agent") != "Qoder" || h.Get("Cosy-Clienttype") != "10" || h.Get("Cosy-Version") != "0.4.3" ||
			h.Get("Cosy-Machinetoken") != "tok-A" || h.Get("Cosy-Machineid") != DeriveID(cred.UID, "machine") ||
			h.Get("Authorization") != "Bearer dt-fake" {
			t.Errorf("%s 头不对：%v", key, h)
		}
	}
	q := rp.hdrs["GET "+oaPathQuota]
	if q.Get("User-Agent") != ConfigFor(RealmCN).UA || q.Get("Cosy-Clienttype") != "" || q.Get("X-Machine-Id") == "" {
		t.Errorf("额度请求应为普通头：%v", q)
	}
}

// TestCheckinWithoutIdentityExplains 没有组件时：不发机器头，并在结论里说明原因（不静默降级）。
func TestCheckinWithoutIdentityExplains(t *testing.T) {
	v := acctLoadVectors(t)
	var routes map[string][2]any
	for _, sc := range v.Checkin {
		if sc.Name == "no_campaigns" {
			routes = sc.Routes
		}
	}
	rp := &acctReplay{routes: routes}
	c := acctNewClient(t, rp)
	for _, realm := range []string{RealmCN, RealmIntl} {
		cred := Cred{UID: "u1", Nickname: "n", Realm: realm, AccessToken: "dt-x"}
		res, err := c.RunCheckin(context.Background(), cred, true, CheckinOptions{Gap: -1})
		if err != nil {
			t.Fatal(err)
		}
		if res.MachineHeaders != MachineHeadersOmitted || res.Identity != IdentitySourceDerived || res.IdentityError == "" {
			t.Errorf("%s 身份状态：%+v", realm, res)
		}
		if !strings.Contains(res.Message, "当前账号暂无可领取的官方活动") || !strings.Contains(res.Message, "未找到官方 UMID 组件") {
			t.Errorf("%s 结论应说明身份降级原因：%q", realm, res.Message)
		}
		if realm == RealmIntl && !strings.Contains(res.Message, "国际版服务端要求真实的 UMID 机器身份") {
			t.Errorf("国际版应带已知限制提示：%q", res.Message)
		}
		if len(res.Logs) < 2 || !strings.Contains(res.Logs[1], "未取得官方机器身份") {
			t.Errorf("日志应记录身份降级：%v", res.Logs)
		}
		if h := rp.hdrs["GET "+oaPathCampaigns]; h.Get("Cosy-Machinetoken") != "" || h.Get("Cosy-Machineid") != "" || h.Get("Cosy-Clienttype") != "10" {
			t.Errorf("无身份时一个机器头都不能发：%v", h)
		}
	}
	_, err := c.MachineIdentity(context.Background())
	if !errors.Is(err, ErrRuntimeInfoNotFound) {
		t.Errorf("err=%v", err)
	}
}

// TestClaimProMatchesPython Pro 福利包：领取成功 / 不可领 / 409 已领 / 资格 404 / 资格 500 / 领取失败。
func TestClaimProMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	for _, sc := range v.Pro {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			c := acctNewClient(t, &acctReplay{routes: sc.Routes})
			cred := Cred{UID: "u-test-0001", Nickname: "tester", Realm: RealmCN, AccessToken: "dt-fake"}
			res, err := c.claimPro(context.Background(), cred, CheckinOptions{Gap: -1})
			if res.OK != sc.OK || res.Earned != sc.Earned || res.AlreadyClaimed != sc.Already || res.State != sc.State || res.Message != sc.Msg {
				t.Errorf("got ok=%v earned=%v already=%v state=%q msg=%q err=%v\nwant ok=%v earned=%v already=%v state=%q msg=%q",
					res.OK, res.Earned, res.AlreadyClaimed, res.State, res.Message, err, sc.OK, sc.Earned, sc.Already, sc.State, sc.Msg)
			}
			if strings.Join(res.Logs, "\n") != strings.Join(sc.Logs, "\n") {
				t.Errorf("Logs\n got %q\nwant %q", res.Logs, sc.Logs)
			}
			if sc.Name == "pro_eligibility_500" && (err == nil || err.Kind != KindServer) {
				t.Errorf("资格查询 5xx 应上报：%v", err)
			}
		})
	}
}

// TestCampaignsParse 活动字段映射、显示名兜底、状态归一、跳转链接与进度。
func TestCampaignsParse(t *testing.T) {
	body := `{"showCampaign":true,"claimable":true,"campaignUrl":"https://qoder.com.cn/activities","campaigns":[
	 {"campaignId":"c1","campaignKey":"act-20260930","claimStatus":"CLAIMABLE","actionType":"CLAIM_BENEFIT",
	  "benefit":{"kind":"CREDITS","amount":100},"startAt":1790000000000,"endAt":"1890000000",
	  "placements":[{"campaignUrl":"https://x/pl","content":{"zh":{"title":"","description":" 说明 "},"en":{"title":"Daily","detailUrl":"https://docs/d"}}}]},
	 {"campaignId":"c2","campaignKey":"k2","claimStatus":"NOT_ELIGIBLE","unavailableReason":"REDEMPTION_CODE_OUT_OF_STOCK",
	  "benefit":{"kind":"REDEMPTION_CODE","amount":1}},
	 {"campaignId":"c3","claimStatus":"ACHIEVEMENT_NOT_COMPLETED","requiredAchievementKey":"sites_first_use","achievementCompleted":false,
	  "benefit":{"kind":"COUPON"}},
	 "garbage"]}`
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	l, err := c.CampaignsView(context.Background(), Cred{UID: "u", AccessToken: "dt"})
	if err != nil || len(l.Campaigns) != 3 || !l.ShowCampaign || !l.Claimable || !l.Available {
		t.Fatalf("list=%+v err=%v", l, err)
	}
	a, b, d := l.Campaigns[0], l.Campaigns[1], l.Campaigns[2]
	if a.Title != "每天领 100 Credits" || a.Label != "act-20260930" || a.TitleEn != "Daily" || a.Description != "说明" ||
		a.DetailURL != "https://docs/d" || a.JumpURL != "https://docs/d" || a.StartAt != 1790000000 || a.EndAt != 1890000000 ||
		a.State != "claimable" || a.StateLabel != "可领" || a.Current != 1 || a.Target != 1 || a.RewardCredit != 100 {
		t.Errorf("c1=%+v", a)
	}
	if b.State != "out_of_stock" || b.RewardText != "兑换码 ×1" || b.RewardCredit != 0 || b.JumpURL != "https://qoder.com.cn/activities" ||
		b.Title != "限时活动（兑换码）" || b.Current != 0 {
		t.Errorf("c2=%+v", b)
	}
	if d.State != "task_required" || d.RequiredAchievementKey != "sites_first_use" || d.RewardText != "优惠券 ×1" || d.Title != "限时活动（优惠券）" {
		t.Errorf("c3=%+v", d)
	}
}

// TestCampaignsSelfHeal showCampaign=false 且带着真实身份 → 清种子换新身份重试一次。
func TestCampaignsSelfHeal(t *testing.T) {
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cosy-Machinetoken") == "tok-B" {
			_, _ = io.WriteString(w, `{"showCampaign":true,"campaigns":[{"campaignId":"c1","claimStatus":"CLAIMABLE"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"showCampaign":false,"campaigns":[]}`)
	}))
	acctWithIdentity(t, c, "tok-A")
	// 种子被删后组件给出新身份（模拟官方组件「删种子必换新」的行为）。
	seed := c.identity.seedPath()
	if err := acctWriteFile(seed, "A"); err != nil {
		t.Fatal(err)
	}
	c.identity.run = func(ctx context.Context, exe string, stdin []byte) ([]byte, error) {
		tok := "tok-A"
		if !acctFileExists(seed) {
			tok = "tok-B"
		}
		return []byte(`{"machineToken":"` + tok + `","machineType":"t","machineCode":"m"}`), nil
	}
	l, err := c.CampaignsView(context.Background(), Cred{UID: "u", AccessToken: "dt"})
	if err != nil || !l.Healed || !l.ShowCampaign || len(l.Campaigns) != 1 || l.Identity != IdentitySourceNative {
		t.Fatalf("自愈失败：%+v %v", l, err)
	}
	if id, _ := c.MachineIdentity(context.Background()); id == nil || id.MachineToken != "tok-B" {
		t.Errorf("自愈后应使用新身份：%+v", id)
	}
}
