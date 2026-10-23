package panel

// 面板的订阅 / 内核 / 平台出口接口。内核本身的启停与热重载由 internal/proxycore 用假内核测；
// 这里用一个没装内核的管理器，验证接口层：鉴权要求、输入校验、订阅地址不下发、平台出口设置。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/proxycore"
)

const panelClashSub = "proxies:\n  - {\"name\":\"US 001\",\"type\":\"trojan\",\"server\":\"a.example\",\"port\":443,\"password\":\"pw\"}\n"

func corePanel(t *testing.T) (*Panel, *proxycore.Manager) {
	t.Helper()
	p, _, _, _ := proxyPoolPanel(t)
	m, err := proxycore.New(proxycore.Options{Dir: t.TempDir(), BinPath: filepath.Join(t.TempDir(), "no-mihomo"), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	p.cfg.ProxyCore = m
	p.cfg.Kiro.NodeURLs = m.NodeURLs
	return p, m
}

func TestCoreEndpoints(t *testing.T) {
	p, _ := corePanel(t)
	code, out := kiroDo(t, p, "GET", "/panel/api/proxycore", "")
	st, _ := out["status"].(map[string]any)
	if code != 200 || st == nil || st["installed"] != false || st["running"] != false {
		t.Fatalf("status: %d %v", code, out)
	}

	// 订阅 ID 进文件路径：路径里转义过的 ../ 也必须挡住（Go 的路由会把 %2F 解码进 PathValue）。
	for _, path := range []string{
		"/panel/api/proxycore/subs/..%2F..%2Fvictim/content",
		"/panel/api/proxycore/subs/..%2F..%2Fvictim/refresh",
		"/panel/api/proxycore/subs/..%2F..%2Fvictim/remove",
		"/panel/api/proxycore/subs/ABCDEFGH/select",
	} {
		if code, _ := kiroDo(t, p, "POST", path, `{"content":"proxies:\n","nodes":[]}`); code != 404 {
			t.Errorf("%s: want 404, got %d", path, code)
		}
	}

	// 内核没装：启动失败；上传垃圾被拒。
	if code, out = kiroDo(t, p, "POST", "/panel/api/proxycore/start", ""); code != 502 || !strings.Contains(out["error"].(string), "没安装") {
		t.Errorf("start without a core: %d %v", code, out)
	}
	if code, out = kiroDo(t, p, "POST", "/panel/api/proxycore/upload", "definitely not mihomo"); code != 400 || !strings.Contains(out["error"].(string), "不认识的文件") {
		t.Errorf("upload garbage: %d %v", code, out)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/install", `{"mirror":"http://insecure.example/"}`); code != 400 {
		t.Errorf("a plain-http mirror must be refused: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/bogus", ""); code != 404 {
		t.Errorf("unknown action: %d", code)
	}

	// 订阅：粘贴内容添加；地址不下发；坏输入 400。
	body, _ := json.Marshal(map[string]string{"name": "机场", "content": panelClashSub})
	code, out = kiroDo(t, p, "POST", "/panel/api/proxycore/subs", string(body))
	sub, _ := out["sub"].(map[string]any)
	if code != 200 || sub == nil || sub["has_content"] != true || sub["format"] != "clash" {
		t.Fatalf("add pasted: %d %v", code, out)
	}
	id := sub["id"].(string)
	// 真实订阅动辄几十 KB（实测 53 个节点 77 KB）：超过通用 JSON 读取的 64 KB 也要能存。
	var big strings.Builder
	big.WriteString("proxies:\n")
	for i := 0; big.Len() < 200<<10; i++ {
		fmt.Fprintf(&big, "  - {\"name\":\"node %04d with a fairly long name\",\"type\":\"trojan\",\"server\":\"h%d.example\",\"port\":443,\"password\":\"%s\"}\n", i, i, strings.Repeat("p", 40))
	}
	body, _ = json.Marshal(map[string]string{"content": big.String()})
	if code, out = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/content", string(body)); code != 200 {
		t.Errorf("a 200 KB subscription must be accepted: %d %v", code, out)
	}
	for _, bad := range []string{`{}`, `{"url":"ftp://x/y"}`, `{"content":"hello"}`, `not json`} {
		if code, _ := kiroDo(t, p, "POST", "/panel/api/proxycore/subs", bad); code != 400 {
			t.Errorf("add %q: want 400, got %d", bad, code)
		}
	}
	// 只有地址的订阅：拉取失败（地址不可达）但订阅保留，带 warning。
	code, out = kiroDo(t, p, "POST", "/panel/api/proxycore/subs", `{"url":"https://127.0.0.1:1/s/secret-token"}`)
	if code != 200 || out["warning"] == nil {
		t.Fatalf("unreachable url: %d %v", code, out)
	}
	_, list := kiroDo(t, p, "GET", "/panel/api/proxycore", "")
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "secret-token") || len(list["subs"].([]any)) != 2 {
		t.Errorf("subscription URL leaked or list wrong: %s", raw)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs", `{"url":"https://127.0.0.1:1/s/secret-token"}`); code != 400 {
		t.Errorf("a duplicate URL must be refused: %d", code)
	}

	// 节点要内核在跑；改名、改间隔、粘贴新内容、删除。
	if code, _ = kiroDo(t, p, "GET", "/panel/api/proxycore/subs/"+id+"/nodes", ""); code != 409 {
		t.Errorf("nodes without a running core: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/update", `{"name":"新名字","interval_min":10}`); code != 400 {
		t.Errorf("an interval below the minimum must be refused: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/update", `{"name":"新名字","interval_min":120}`); code != 200 {
		t.Errorf("update: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/content", `{"content":"garbage"}`); code != 400 {
		t.Errorf("bad content: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/refresh", ""); code != 502 {
		t.Errorf("refreshing a pasted-only subscription must fail: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/select", `{"nodes":["US 001"]}`); code != 200 {
		t.Errorf("select: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/"+id+"/remove", ""); code != 200 {
		t.Errorf("remove: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/nope/remove", ""); code != 404 {
		t.Errorf("remove a malformed id: %d", code)
	}
	if code, _ = kiroDo(t, p, "POST", "/panel/api/proxycore/subs/0123abcd/remove", ""); code != 400 {
		t.Errorf("remove an unknown id: %d", code)
	}
}

// 面板没设 api_key 时，安装 / 上传 / 启动内核一律拒绝（等于允许任何人在服务器上运行程序）。
func TestCoreRequiresAPIKey(t *testing.T) {
	p, _ := corePanel(t)
	p.cfg.APIKey = ""
	for _, path := range []string{"/panel/api/proxycore/install", "/panel/api/proxycore/upload", "/panel/api/proxycore/start", "/panel/api/proxycore/restart",
		"/panel/api/proxycore/subs", "/panel/api/proxycore/subs/0123abcd/refresh", "/panel/api/proxycore/subs/0123abcd/content",
		"/panel/api/proxycore/subs/0123abcd/update", "/panel/api/proxycore/subs/0123abcd/select"} {
		code, out := kiroDo(t, p, "POST", path, `{}`)
		if code != 403 || !strings.Contains(out["error"].(string), "api_key") {
			t.Errorf("%s without api_key: %d %v", path, code, out)
		}
	}
	if code, _ := kiroDo(t, p, "POST", "/panel/api/proxycore/stop", ""); code != 200 {
		t.Errorf("stopping must always be allowed: %d", code)
	}
}

func TestEgressEndpoint(t *testing.T) {
	p, _ := corePanel(t)
	rowsOf := func() map[string]map[string]any {
		_, out := kiroDo(t, p, "GET", "/panel/api/kiro/proxies", "")
		m := map[string]map[string]any{}
		for _, x := range out["platforms"].([]any) {
			r := x.(map[string]any)
			m[r["platform"].(string)] = r
		}
		return m
	}
	rows := rowsOf()
	if wb := rows["workbuddy"]; wb == nil || wb["mode"] != "" || wb["direct_cn"] != true || wb["current"] != nil || rows["qoder"] == nil {
		t.Fatalf("defaults: %v", rows)
	}
	// 自动，但池是空的：显示错误。
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/egress", `{"platform":"workbuddy","mode":"auto","direct_cn":true}`); code != 200 {
		t.Fatalf("set auto: %d", code)
	}
	if wb := rowsOf()["workbuddy"]; wb["mode"] != "auto" || !strings.Contains(wb["error"].(string), "代理池是空的") {
		t.Errorf("auto with an empty pool: %v", wb)
	}
	// 加一个带密码的代理：当前出口只显示脱敏地址。
	kiroDo(t, p, "POST", "/panel/api/kiro/proxies", `{"text":"http://user:secret@p1.example:8080"}`)
	_, out := kiroDo(t, p, "GET", "/panel/api/kiro/proxies", "")
	raw, _ := json.Marshal(out)
	if wb := rowsOf()["workbuddy"]; wb["current"] != "http://p1.example:8080" || strings.Contains(string(raw), "secret") {
		t.Errorf("auto with a member: %v (leak check on %s)", wb, raw)
	}
	id := out["proxies"].([]any)[0].(map[string]any)["id"].(string)
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/egress", `{"platform":"qoder","mode":"pin","proxy_id":"`+id+`","direct_cn":false}`); code != 200 {
		t.Errorf("pin: %d", code)
	}
	if qd := rowsOf()["qoder"]; qd["mode"] != "pin" || qd["proxy_id"] != id || qd["direct_cn"] != false || qd["current"] != "http://p1.example:8080" {
		t.Errorf("pinned: %v", qd)
	}
	for _, bad := range []string{
		`{"platform":"kiro","mode":"auto"}`,
		`{"platform":"qoder","mode":"maybe"}`,
		`{"platform":"qoder","mode":"pin","proxy_id":"nope"}`,
		`not json`,
	} {
		if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/egress", bad); code != 400 {
			t.Errorf("%s: want 400, got %d", bad, code)
		}
	}
	if code, _ := kiroDo(t, p, "POST", "/panel/api/kiro/egress", `{"platform":"workbuddy","mode":"off"}`); code != 200 {
		t.Errorf("off: %d", code)
	}
	if wb := rowsOf()["workbuddy"]; wb["mode"] != "" || wb["current"] != nil || wb["error"] != nil {
		t.Errorf("after off: %v", wb)
	}
}
