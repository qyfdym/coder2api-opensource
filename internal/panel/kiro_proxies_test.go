package panel

// Kiro 代理池页的后端：热增删启停、脱敏、账号出口随之变化、测试、配置文件成员只读。

import (
	"context"
	"encoding/json"
	"net"
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

func proxyPoolPanel(t *testing.T, configPool ...string) (*Panel, *pool.Pool, *kiroacct.Service, string) {
	t.Helper()
	rt := proxyUsageRT()
	rt.on("q.us-east-1.amazonaws.com/", func(*http.Request, []byte) (int, string) { return 403, `{}` })
	rt.on("ipinfo.io/json", func(*http.Request, []byte) (int, string) {
		return 200, `{"ip":"1.2.3.4","country":"US","org":"AS1 Example"}`
	})
	pl := pool.New("")
	svc := &kiroacct.Service{
		Client: kiro.NewClient(kiro.Options{Transport: rt, Logf: t.Logf, ProxyPool: configPool,
			Sleep: func(context.Context, time.Duration) error { return nil }}),
		AuthDir: t.TempDir(), LocalCacheDir: t.TempDir(),
	}
	file := filepath.Join(t.TempDir(), "kiro-proxies.json")
	svc.Proxies, _ = kiroacct.OpenProxyStore(file)
	return New(Config{Version: "test", APIKey: "test-key", Pool: pl, Kiro: svc}), pl, svc, file
}

func proxyRows(t *testing.T, p *Panel) (rows []map[string]any, raw string) {
	t.Helper()
	code, out := kiroDo(t, p, "GET", "/panel/api/kiro/proxies", "")
	if code != 200 {
		t.Fatalf("list: %d %v", code, out)
	}
	b, _ := json.Marshal(out)
	for _, x := range out["proxies"].([]any) {
		rows = append(rows, x.(map[string]any))
	}
	return rows, string(b)
}

func TestKiroProxyPoolLifecycle(t *testing.T) {
	p, pl, _, file := proxyPoolPanel(t)
	uid := importKsk(t, p)

	if rows, _ := proxyRows(t, p); len(rows) != 0 {
		t.Fatalf("fresh pool: %v", rows)
	}
	if row := accountRow(t, p, uid); row["proxy_kind"] != "none" {
		t.Fatalf("no pool yet: %v", row)
	}

	// 一次粘贴多行：带账号密码和备注的、没写协议的、一行写错的。
	pl.BlockModelBackoff(uid, "claude-sonnet-4.5", "kiro model unavailable for this account")
	body, _ := json.Marshal(map[string]string{
		"text":   "socks5://user:secret@us1.example:1080 美国一号\nus2.example:8080\nthis is not a proxy\n",
		"scheme": "http",
	})
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/proxies", string(body))
	if code != 200 || out["added"] != float64(2) || out["moved"] != float64(1) || len(out["skipped"].([]any)) != 1 || len(out["ids"].([]any)) != 2 {
		t.Fatalf("add: %d %v", code, out)
	}
	// 账号的出口变了 → 按旧出口判的「模型不可用」作废。
	if st := pl.ModelBlocked("claude-sonnet-4.5"); st.Blocked {
		t.Errorf("moving the egress must clear the model block: %+v", st)
	}
	row := accountRow(t, p, uid)
	if row["proxy_kind"] != "pool" {
		t.Fatalf("account must now use the pool: %v", row)
	}

	rows, raw := proxyRows(t, p)
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	if strings.Contains(raw, "secret") || strings.Contains(raw, "user:") {
		t.Errorf("credentials leaked in the list: %s", raw)
	}
	var used, other map[string]any
	for _, r := range rows {
		if r["accounts"] == float64(1) {
			used = r
		} else {
			other = r
		}
		if r["source"] != "panel" || r["enabled"] != true || r["active"] != true {
			t.Errorf("row state: %v", r)
		}
	}
	if used == nil || other == nil || used["proxy"] != row["proxy"] {
		t.Fatalf("exactly one proxy must carry the account: used=%v other=%v row=%v", used, other, row["proxy"])
	}
	for _, r := range rows {
		if strings.HasPrefix(r["proxy"].(string), "socks5://us1") && (r["auth"] != true || r["note"] != "美国一号") {
			t.Errorf("credentialed row: %v", r)
		}
	}

	// 停用正在用的那个 → 账号立刻换到另一个；全部停用 → 回到没有代理。
	base := "/panel/api/kiro/proxies/"
	code, out = kiroDo(t, p, "POST", base+used["id"].(string)+"/enabled", `{"enabled":false}`)
	if code != 200 || out["moved"] != float64(1) {
		t.Fatalf("disable: %d %v", code, out)
	}
	if row = accountRow(t, p, uid); row["proxy"] != other["proxy"] {
		t.Errorf("account must fail over to the other proxy: %v", row)
	}
	if code, out = kiroDo(t, p, "POST", base+other["id"].(string)+"/enabled", `{"enabled":false}`); code != 200 || out["moved"] != float64(1) {
		t.Fatalf("disable second: %d %v", code, out)
	}
	if row = accountRow(t, p, uid); row["proxy_kind"] != "none" {
		t.Errorf("empty pool: %v", row)
	}
	// 停用的代理仍然能测试。
	if code, out = kiroDo(t, p, "POST", base+used["id"].(string)+"/test", ""); code != 200 {
		t.Fatalf("test: %d %v", code, out)
	} else if tr := out["test"].(map[string]any); tr["ok"] != true || tr["country"] != "US" {
		t.Errorf("test result: %v", tr)
	}
	rows, _ = proxyRows(t, p)
	for _, r := range rows {
		if r["id"] == used["id"] && (r["test"] == nil || r["enabled"] != false || r["active"] != false) {
			t.Errorf("disabled+tested row: %v", r)
		}
	}

	// 重新启用 → 账号回来；删除 → 落盘的文件里也没了。
	if code, _ = kiroDo(t, p, "POST", base+used["id"].(string)+"/enabled", `{"enabled":true}`); code != 200 {
		t.Fatalf("re-enable: %d", code)
	}
	if row = accountRow(t, p, uid); row["proxy"] != used["proxy"] {
		t.Errorf("re-enabled proxy must serve the account again: %v", row)
	}
	if code, _ = kiroDo(t, p, "POST", base+used["id"].(string)+"/remove", ""); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	usedLabel, otherLabel := used["proxy"].(string), other["proxy"].(string)
	usedHost, otherHost := usedLabel[strings.Index(usedLabel, "://")+3:], otherLabel[strings.Index(otherLabel, "://")+3:]
	if b, _ := os.ReadFile(file); strings.Contains(string(b), usedHost) || !strings.Contains(string(b), otherHost) {
		t.Errorf("removal must be persisted (removed %s, kept %s): %s", usedHost, otherHost, b)
	}
	if rows, _ = proxyRows(t, p); len(rows) != 1 {
		t.Errorf("after remove: %v", rows)
	}
}

func TestKiroProxyPoolRejectsBadRequests(t *testing.T) {
	p, _, _, _ := proxyPoolPanel(t)
	base := "/panel/api/kiro/proxies"
	for _, body := range []string{`{"text":""}`, `{"text":"nonsense\nmore nonsense"}`, `not json`} {
		if code, _ := kiroDo(t, p, "POST", base, body); code != 400 {
			t.Errorf("add %q: want 400, got %d", body, code)
		}
	}
	// 「测试服务器直连」：不经代理，结果记在列表里。
	if code, out := kiroDo(t, p, "POST", base+"/direct/test", ""); code != 200 || out["test"].(map[string]any)["ok"] != true {
		t.Errorf("direct test: %d %v", code, out)
	}
	if code, out := kiroDo(t, p, "GET", base, ""); code != 200 || out["direct_test"] == nil {
		t.Errorf("the direct test result must be listed: %d %v", code, out)
	}
	for _, path := range []string{"/enabled", "/remove", "/test"} {
		if code, _ := kiroDo(t, p, "POST", base+"/nope"+path, `{"enabled":true}`); code != 404 {
			t.Errorf("unknown id %s: want 404, got %d", path, code)
		}
	}
	if code, _ := kiroDo(t, p, "POST", base+"/nope/enabled", `{}`); code != 400 {
		t.Errorf("enabled without a value: want 400, got %d", code)
	}
}

// 配置文件里的 proxy_pool 在页面上只读：能看、能测，不能停用 / 删除，重复粘贴不会再托管一份。
func TestKiroProxyPoolConfigMembersReadOnly(t *testing.T) {
	p, _, _, _ := proxyPoolPanel(t, "http://u:pw@cfg.example:7890")
	rows, raw := proxyRows(t, p)
	if len(rows) != 1 || rows[0]["source"] != "config" || rows[0]["proxy"] != "http://cfg.example:7890" || rows[0]["active"] != true {
		t.Fatalf("rows: %v", rows)
	}
	if strings.Contains(raw, "pw@") || strings.Contains(raw, "u:pw") {
		t.Errorf("config pool credentials leaked: %s", raw)
	}
	id := rows[0]["id"].(string)
	base := "/panel/api/kiro/proxies/" + id
	if code, _ := kiroDo(t, p, "POST", base+"/enabled", `{"enabled":false}`); code != 404 {
		t.Errorf("disable config member: want 404, got %d", code)
	}
	if code, _ := kiroDo(t, p, "POST", base+"/remove", ""); code != 404 {
		t.Errorf("remove config member: want 404, got %d", code)
	}
	if code, out := kiroDo(t, p, "POST", base+"/test", ""); code != 200 || out["test"].(map[string]any)["ok"] != true {
		t.Errorf("test config member: %d %v", code, out)
	}
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/proxies", `{"text":"http://u:pw@cfg.example:7890\nhttp://new.example:1"}`)
	if code != 200 || out["added"] != float64(1) || out["existing"] != float64(1) {
		t.Errorf("duplicate of a config member: %d %v", code, out)
	}
	if rows, _ = proxyRows(t, p); len(rows) != 2 {
		t.Errorf("rows after add: %v", rows)
	}
}

// 账号自己指定了代理时，池的增删不改它的出口，也不会作废它的目录。
func TestKiroProxyPoolLeavesAccountProxyAlone(t *testing.T) {
	p, _, _, _ := proxyPoolPanel(t)
	uid := importKsk(t, p)
	kiroDo(t, p, "POST", "/panel/api/kiro/accounts/"+uid+"/proxy", `{"proxy_url":"http://own.example:1"}`)
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/proxies", `{"text":"http://pool.example:1"}`)
	if code != 200 || out["moved"] != float64(0) {
		t.Fatalf("add: %d %v", code, out)
	}
	if row := accountRow(t, p, uid); row["proxy_kind"] != "account" || row["proxy"] != "http://own.example:1" {
		t.Errorf("account proxy must win: %v", row)
	}
}

func TestKiroProxyPoolNeedsStore(t *testing.T) {
	p, _, svc := newKiroPanel2(t, proxyUsageRT())
	if svc.Proxies != nil {
		t.Fatal("precondition")
	}
	if code, _ := kiroDo(t, p, "GET", "/panel/api/kiro/proxies", ""); code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d", code)
	}
}

// 服务器连不上 Kiro 时导入：凭证先保存入池，并明确告诉用户「没校验」，而不是整条导入失败。
type kiroDownRT struct{}

func (kiroDownRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
}

func TestKiroImportWhileKiroUnreachableWarns(t *testing.T) {
	pl := pool.New("")
	svc := &kiroacct.Service{
		Client:  kiro.NewClient(kiro.Options{Transport: kiroDownRT{}, Logf: t.Logf}),
		AuthDir: t.TempDir(),
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, Kiro: svc})
	// 账号管理器导出的 JSON：带 access token，可以先存。
	exported := `[{"email":"me@example.com","userId":"user-off","accessToken":"at-old","refreshToken":"refresh-token-aaaaaaaa",` +
		`"clientId":"cid","clientSecret":"csecret","authMethod":"IdC","provider":"BuilderId","region":"us-east-1","expiresAt":"2020-01-01T00:00:00Z"}]`
	body, _ := json.Marshal(map[string]string{"text": exported})
	code, out := kiroDo(t, p, "POST", "/panel/api/kiro/import", string(body))
	warns, _ := out["warnings"].([]any)
	if code != 200 || out["imported"] != float64(1) || len(warns) != 1 || !strings.Contains(warns[0].(string), "没能联网校验") {
		t.Fatalf("import: %d %v", code, out)
	}
	if failed, _ := out["failed"].([]any); len(failed) != 0 {
		t.Errorf("nothing may be reported as failed: %v", failed)
	}
	if n := len(pl.List()); n != 1 {
		t.Errorf("the account must be in the pool, got %d", n)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "refresh-token-aaaaaaaa") || strings.Contains(string(raw), "csecret") {
		t.Errorf("credentials leaked in the response: %s", raw)
	}

	// 只有 refresh token 的行格式必须联网换 access token：连不上就如实失败，并写明出口。
	line, _ := json.Marshal(map[string]string{"text": "other@example.com----pw----refresh-token-bbbbbbbb----cid----csecret"})
	code, out = kiroDo(t, p, "POST", "/panel/api/kiro/import", string(line))
	failed, _ := out["failed"].([]any)
	if code != 200 || out["imported"] != float64(0) || len(failed) != 1 || !strings.Contains(failed[0].(string), "出口：") {
		t.Fatalf("a refresh-token-only line cannot be saved offline and must name the egress: %d %v", code, out)
	}
}
