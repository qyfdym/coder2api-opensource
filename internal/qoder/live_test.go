package qoder

// 在线冒烟测试：只在 QODER_LIVE=1 时运行，凭证取自 QODER_LIVE_ACCOUNT 指向的本地账号文件
// （Python 版 sidecar 的 accounts/<uid>.json 格式），例如：
//
//	QODER_LIVE=1 QODER_LIVE_ACCOUNT=/path/to/accounts/<uid>.json go test ./internal/qoder -run Live -v
//
// 约束（保护本地账号）：
//   - 只做只读接口与少量极短对话；**绝不**调用 Refresh / ExchangePAT / 登录 / 任何领取接口
//     （刷新会轮换 refresh token，把本地账号文件里的旧 token 作废）。
//   - 任何输出都不打印 token。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func liveCred(t *testing.T) Cred {
	t.Helper()
	if os.Getenv("QODER_LIVE") != "1" {
		t.Skip("set QODER_LIVE=1 to run live Qoder tests")
	}
	file := os.Getenv("QODER_LIVE_ACCOUNT")
	if file == "" {
		t.Skip("set QODER_LIVE_ACCOUNT=<account json path> to run live Qoder tests")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Skipf("no local account file: %v", err)
	}
	var a struct {
		UID, Nickname, Realm, AccessToken, RefreshToken, PersonalToken, UserType string
		OrganizationID, OrganizationName                                         string
		ExpiresAt                                                                int64
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	return Cred{UID: a.UID, Nickname: a.Nickname, Realm: a.Realm, AccessToken: a.AccessToken,
		RefreshToken: a.RefreshToken, PAT: a.PersonalToken, ExpiresAt: a.ExpiresAt,
		UserType: a.UserType, OrgID: a.OrganizationID, OrgName: a.OrganizationName}
}

// liveCollect 读完 OpenAI SSE，返回正文、思考、工具名、finish_reason、usage 帧与是否见到 [DONE]。
func liveCollect(t *testing.T, rc io.ReadCloser) (content, reasoning string, tools []string, finish string, usage map[string]any, done bool, errFrame string) {
	t.Helper()
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			t.Errorf("非 JSON 帧: %.200s", data)
			continue
		}
		if _, ok := f["error"]; ok {
			errFrame = data
			continue
		}
		if u, ok := f["usage"].(map[string]any); ok {
			usage = u
		}
		chs, _ := f["choices"].([]any)
		for _, ch := range chs {
			m, _ := ch.(map[string]any)
			if fr, _ := m["finish_reason"].(string); fr != "" {
				finish = fr
			}
			d, _ := m["delta"].(map[string]any)
			if s, _ := d["content"].(string); s != "" {
				content += s
			}
			if s, _ := d["reasoning_content"].(string); s != "" {
				reasoning += s
			}
			tcs, _ := d["tool_calls"].([]any)
			for _, tc := range tcs {
				tm, _ := tc.(map[string]any)
				fn, _ := tm["function"].(map[string]any)
				if n, _ := fn["name"].(string); n != "" {
					tools = append(tools, n)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Errorf("read stream: %v", err)
	}
	return
}

func TestLiveModels(t *testing.T) {
	cred := liveCred(t)
	c := NewClient(Options{DataDir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ms, err := c.RefreshModels(ctx, cred.Realm, cred)
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("动态目录为空")
	}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.ID+"("+m.Key+")")
	}
	t.Logf("%d models: %s", len(ms), strings.Join(names, ", "))
	if _, ok := c.LookupModel(cred.Realm, "Qwen3.8-Flash"); !ok {
		t.Error("LookupModel(Qwen3.8-Flash) 未命中")
	}
}

func liveChat(t *testing.T, c *Client, cred Cred, body string) (string, string, []string, string, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	rc, qerr := c.ChatStream(ctx, cred, []byte(body))
	if qerr != nil {
		t.Fatalf("ChatStream: %v", qerr)
	}
	content, reasoning, tools, finish, usage, done, errFrame := liveCollect(t, rc)
	if errFrame != "" {
		t.Fatalf("error frame: %s", errFrame)
	}
	if !done {
		t.Error("缺少 [DONE]")
	}
	if usage == nil {
		t.Error("缺少 usage 帧")
	}
	t.Logf("%.1fs finish=%s content=%q reasoning_len=%d tools=%v usage=%v",
		time.Since(start).Seconds(), finish, content, len(reasoning), tools, usage)
	return content, reasoning, tools, finish, usage
}

func TestLiveChatText(t *testing.T) {
	cred := liveCred(t)
	c := NewClient(Options{DataDir: t.TempDir()})
	content, _, _, finish, _ := liveChat(t, c, cred,
		`{"model":"Qwen3.8-Flash","stream":true,"messages":[{"role":"user","content":"只回复两个字：通了"}]}`)
	if !strings.Contains(content, "通") || finish == "" {
		t.Errorf("content=%q finish=%q", content, finish)
	}
}

func TestLiveChatEffort(t *testing.T) {
	cred := liveCred(t)
	c := NewClient(Options{DataDir: t.TempDir()})
	liveChat(t, c, cred,
		`{"model":"Qwen3.8-Flash","stream":false,"reasoning_effort":"high","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"1+1=? 只回复数字"}]}`)
}

func TestLiveChatTool(t *testing.T) {
	cred := liveCred(t)
	c := NewClient(Options{DataDir: t.TempDir()})
	_, _, tools, finish, _ := liveChat(t, c, cred, `{"model":"Qwen3.8-Flash","stream":true,
	 "tools":[{"type":"function","function":{"name":"get_weather","description":"查询城市天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
	 "messages":[{"role":"user","content":"北京天气怎么样？必须调用 get_weather 工具"}]}`)
	if len(tools) == 0 || tools[0] != "get_weather" || finish != "tool_calls" {
		t.Errorf("期望工具调用 get_weather，got tools=%v finish=%q", tools, finish)
	}
}

func TestLiveAccountReadOnly(t *testing.T) {
	cred := liveCred(t)
	c := NewClient(Options{DataDir: t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if u, err := c.UserInfo(ctx, cred); err != nil {
		t.Errorf("UserInfo: %v", err)
	} else {
		t.Logf("userinfo uid_match=%v nick_set=%v type=%s", u.UID == cred.UID, u.Nickname != "", u.UserType)
	}
	if cr, err := c.FetchCredits(ctx, cred); err != nil {
		t.Errorf("FetchCredits: %v", err)
	} else {
		t.Logf("credits remain=%v eff=%v size=%v packages=%d", cr.Remain, cr.RemainEffective, cr.Size, len(cr.Packages))
	}
	if p, err := c.FetchPlan(ctx, cred); err != nil {
		t.Errorf("FetchPlan: %v", err)
	} else {
		t.Logf("plan=%s", p)
	}
	if cs, err := c.Campaigns(ctx, cred); err != nil {
		t.Logf("Campaigns: %v", err)
	} else {
		for _, x := range cs {
			t.Logf("campaign %s state=%s", x.Label, x.State)
		}
	}
	if id, err := c.MachineIdentity(ctx); err != nil {
		t.Logf("MachineIdentity: %v", err)
	} else {
		t.Logf("machine identity real=%v vm=%v", id.Real, id.IsVM)
	}
}
