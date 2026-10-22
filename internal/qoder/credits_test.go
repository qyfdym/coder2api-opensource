package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// acctJSONDiff 递归比较原实现的 credits dict 与 Go 结构的 JSON 形态（数值按 float 比较；
// 原实现 usage_pct 缺失为 null，Go 为 0，视为相等）。
func acctJSONDiff(path string, want, got any) []string {
	switch w := want.(type) {
	case nil:
		if got == nil {
			return nil
		}
		if f, ok := got.(float64); ok && f == 0 {
			return nil
		}
		return []string{fmt.Sprintf("%s: want null got %v", path, got)}
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want object got %T", path, got)}
		}
		var out []string
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			wv, wok := w[k]
			gv, gok := g[k]
			if !wok || !gok {
				out = append(out, fmt.Sprintf("%s.%s: present want=%v got=%v", path, k, wok, gok))
				continue
			}
			out = append(out, acctJSONDiff(path+"."+k, wv, gv)...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return []string{fmt.Sprintf("%s: list len want %d got %v", path, len(w), got)}
		}
		var out []string
		for i := range w {
			out = append(out, acctJSONDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return out
	default:
		if fmt.Sprint(want) != fmt.Sprint(got) {
			return []string{fmt.Sprintf("%s: want %v got %v", path, want, got)}
		}
		return nil
	}
}

// TestQuotaParseMatchesPython 额度解析与原实现 fetch_credits 的结果逐字段一致
// （个人 / 团队共享池 / 共享池不可用 / null / 无限池 / 数值池 / 字符串数字 / 退化体 / 真耗尽）。
func TestQuotaParseMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	for _, c := range v.Credits {
		t.Run(c.Name, func(t *testing.T) {
			got, err := parseQuotaBody(c.Body, time.Now())
			if !c.OK {
				if err == nil || err.Code != "QUOTA_DEGENERATE" || !strings.Contains(err.Msg, c.Reason) {
					t.Fatalf("应判为退化体（%s）：err=%v", c.Reason, err)
				}
				if err.Body == "" {
					t.Error("退化体应保留原文摘要")
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			raw, _ := json.Marshal(got)
			var gm map[string]any
			_ = json.Unmarshal(raw, &gm)
			delete(gm, "updated_at")
			if diffs := acctJSONDiff("credits", c.Credits, gm); len(diffs) > 0 {
				t.Errorf("与原实现不一致：\n%s", strings.Join(diffs, "\n"))
			}
		})
	}
}

func TestQuotaEffectiveRemain(t *testing.T) {
	team, err := parseQuotaBody([]byte(`{"userQuota":{"remaining":0,"total":1000,"used":1000},
		"orgResourcePackage":{"remaining":952,"used":1548,"cap":2500,"available":true}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if team.Remain != 0 || team.RemainEffective != 952 || !team.HasBalance() || team.Shared == nil || team.Shared.Source != "orgResourcePackage" {
		t.Errorf("团队共享池应计入有效余额：%+v", team)
	}
	unl, _ := parseQuotaBody([]byte(`{"userQuota":{"remaining":0,"total":10},"orgResourcePackage":{"remaining":5,"cap":-1}}`), time.Now())
	if !unl.SharedUnlimited() || unl.RemainEffective != 0 || !unl.HasBalance() {
		t.Errorf("无限池：不计入有效余额但视为有额度：%+v", unl)
	}
	if _, err := parseQuotaBody([]byte(`[1,2]`), time.Now()); err == nil || !strings.Contains(err.Msg, "non-dict") {
		t.Errorf("非对象响应应判退化：%v", err)
	}
}

// TestFetchCreditsDesktopRetryAndRealmHint 403 → 桌面端头重试；仍失败 → 探测另一区并给出提示。
func TestFetchCreditsDesktopRetryAndRealmHint(t *testing.T) {
	acctFastRetry(t)
	mode := "retry-ok"
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oaPathQuota {
			w.WriteHeader(404)
			return
		}
		desktop := r.Header.Get("Cosy-Clienttype") == "10" && r.Header.Get("User-Agent") == "Qoder"
		cn := r.Header.Get("X-Orig-Host") == "openapi.qoder.com.cn"
		switch {
		case mode == "retry-ok" && desktop:
			_, _ = io.WriteString(w, `{"userQuota":{"remaining":5,"total":10,"used":5}}`)
		case mode == "wrong-realm" && !cn:
			_, _ = io.WriteString(w, `{"userQuota":{"remaining":5,"total":10,"used":5}}`)
		default:
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `<html>WAF</html>`)
		}
	}))
	cred := Cred{UID: "u", Realm: "cn", AccessToken: "dt-x"}
	got, err := c.FetchCredits(context.Background(), cred)
	if err != nil || got.Remain != 5 {
		t.Fatalf("桌面端头重试应成功：%+v %v", got, err)
	}
	mode = "wrong-realm"
	_, err = c.FetchCredits(context.Background(), cred)
	if err == nil || err.Kind != KindAuth || !strings.Contains(err.Msg, "区域标记错误") || !strings.Contains(err.Msg, "国际版") {
		t.Fatalf("应提示区域标记错误：%v", err)
	}
}

func TestFetchCreditsSessionDeadNoRetry(t *testing.T) {
	n := 0
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"code":"TOKEN_EXPIRE"}`)
	}))
	_, err := c.FetchCredits(context.Background(), Cred{UID: "u", AccessToken: "dt-x"})
	if err == nil || err.Kind != KindSessionDead || n != 1 {
		t.Fatalf("会话死亡不应再用桌面端头重试：%v calls=%d", err, n)
	}
}

func TestFetchPlan(t *testing.T) {
	body := `{"plan_tier_name":"Pro Trial","user_type":"x"}`
	c := acctNewClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != oaPathPlan || r.Header.Get("Authorization") != "Bearer dt-x" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	cred := Cred{UID: "u", AccessToken: "dt-x"}
	if p, err := c.FetchPlan(context.Background(), cred); err != nil || p != "Pro Trial" {
		t.Errorf("plan=%q err=%v", p, err)
	}
	body = `{"user_type":"personal_free"}`
	if p, _ := c.FetchPlan(context.Background(), cred); p != "personal_free" {
		t.Errorf("缺 plan_tier_name 应退回 user_type：%q", p)
	}
}

// TestNextCheckinWindowMatchesPython 「每日 10:00（UTC+8）」窗口（含恰好 10:00 的边界）。
func TestNextCheckinWindowMatchesPython(t *testing.T) {
	v := acctLoadVectors(t)
	for _, c := range v.NextWindow {
		ts := c[0].(float64)
		sec := int64(ts)
		now := time.Unix(sec, int64((ts-float64(sec))*1e9))
		got := NextCheckinWindow(now)
		if got.Unix() != int64(c[1].(float64)) || CheckinWindowNote(got) != c[2].(string) {
			t.Errorf("now=%v got %d %q want %v %v", ts, got.Unix(), CheckinWindowNote(got), c[1], c[2])
		}
	}
}
