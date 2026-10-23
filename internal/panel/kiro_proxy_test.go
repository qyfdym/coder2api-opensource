package panel

// Kiro 账号出口代理：设置 / 清除 / 直连、校验、脱敏、落盘，以及换出口后作废按旧出口算的状态。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func importKsk(t *testing.T, p *Panel) string {
	t.Helper()
	_, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", `{"text":"ksk_proxy"}`)
	accs, _ := out["accounts"].([]any)
	if len(accs) != 1 {
		t.Fatalf("import: %v", out)
	}
	return accs[0].(map[string]any)["uid"].(string)
}

func accountRow(t *testing.T, p *Panel, uid string) map[string]any {
	t.Helper()
	_, ov := kiroDo(t, p, "GET", "/panel/api/overview", "")
	for _, x := range ov["accounts"].([]any) {
		if row := x.(map[string]any); row["uid"] == uid {
			return row
		}
	}
	t.Fatalf("account %s not in overview", uid)
	return nil
}

func proxyUsageRT() *kiroFakeRT {
	rt := &kiroFakeRT{}
	rt.on(kiroUsageRoute, func(*http.Request, []byte) (int, string) {
		return 200, kiroUsageBody("user-proxy", "p@example.com", 5, 50)
	})
	return rt
}

func TestKiroProxyEndpoint(t *testing.T) {
	p, pl, svc := newKiroPanel2(t, proxyUsageRT())
	uid := importKsk(t, p)

	// 没配代理池 / 默认代理：不显示出口。
	if row := accountRow(t, p, uid); row["proxy_kind"] != "none" {
		t.Fatalf("fresh account: %v", row)
	}

	// 账号自己的代理：返回与总览里都只有脱敏地址，账号文件里是完整地址。
	const full = "socks5://user:secret@10.0.0.1:1080"
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":"`+full+`"}`)
	if code != 200 || out["proxy_kind"] != "account" || out["proxy"] != "socks5://10.0.0.1:1080" {
		t.Fatalf("set: %d %v", code, out)
	}
	raw, _ := json.Marshal(accountRow(t, p, uid))
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "user:") {
		t.Errorf("credentials leaked through the overview: %s", raw)
	}
	if row := accountRow(t, p, uid); row["proxy_kind"] != "account" || row["proxy"] != "socks5://10.0.0.1:1080" {
		t.Errorf("overview row: %v", row)
	}
	files, _ := filepath.Glob(filepath.Join(svc.AuthDir, "kiro-*.json"))
	if len(files) != 1 {
		t.Fatalf("account files: %v", files)
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), `"proxyUrl": "`+full+`"`) && !strings.Contains(string(b), `"proxyUrl":"`+full+`"`) {
		t.Errorf("proxy was not persisted in the account file")
	}

	// direct / 清空。
	if code, out = kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":"direct"}`); code != 200 || out["proxy_kind"] != "direct" {
		t.Errorf("direct: %d %v", code, out)
	}
	if code, out = kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":""}`); code != 200 || out["proxy_kind"] != "none" {
		t.Errorf("clear: %d %v", code, out)
	}

	// 非法输入一律 400 且不改动已有设置；未知账号 404。
	kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":"http://127.0.0.1:7890"}`)
	for _, body := range []string{`{}`, `{"proxy_url":"ftp://h:21"}`, `{"proxy_url":"127.0.0.1:7890"}`, `{"proxy_url":"http://nohostport"}`, `not json`} {
		if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", body); code != 400 {
			t.Errorf("body %q: want 400, got %d", body, code)
		}
	}
	if row := accountRow(t, p, uid); row["proxy"] != "http://127.0.0.1:7890" {
		t.Errorf("a rejected request must not change the proxy: %v", row)
	}
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/accounts/nope/proxy", `{"proxy_url":""}`); code != 404 {
		t.Errorf("unknown account: want 404, got %d", code)
	}

	// 换出口：按旧出口判出来的「模型对该号不可用」立即作废。
	pl.BlockModelBackoff(uid, "claude-sonnet-4.5", "kiro model unavailable for this account")
	if st := pl.ModelBlocked("claude-sonnet-4.5"); !st.Blocked {
		t.Fatalf("precondition: model must be blocked")
	}
	kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":"http://127.0.0.1:7891"}`)
	if st := pl.ModelBlocked("claude-sonnet-4.5"); st.Blocked {
		t.Errorf("changing the egress must clear the model block: %+v", st)
	}
}

// 配了代理池：没有自己代理的账号显示池分配的出口。
func TestKiroProxyPoolShownInOverview(t *testing.T) {
	rt := proxyUsageRT()
	pl := pool.New("")
	svc := &kiroacct.Service{
		Client: kiro.NewClient(kiro.Options{Transport: rt, Logf: t.Logf,
			ProxyPool: []string{"http://u:pw@us1.example:7890", "http://us2.example:7890"},
			Sleep:     func(context.Context, time.Duration) error { return nil }}),
		AuthDir: t.TempDir(), LocalCacheDir: t.TempDir(),
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Kiro: svc})
	uid := importKsk(t, p)

	row := accountRow(t, p, uid)
	label, _ := row["proxy"].(string)
	if row["proxy_kind"] != "pool" || (label != "http://us1.example:7890" && label != "http://us2.example:7890") {
		t.Fatalf("pool-assigned row: %v", row)
	}
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), "pw@") || strings.Contains(string(raw), "u:pw") {
		t.Errorf("pool credentials leaked: %s", raw)
	}
}
