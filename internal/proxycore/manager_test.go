package proxycore

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const clashSub = `mixed-port: 7890
proxies:
  - {"name":"US 001","type":"trojan","server":"a.example","port":443,"password":"pw"}
  - {"name":"JP 002","type":"trojan","server":"b.example","port":443,"password":"pw"}
  - {"name":"HK 003","type":"vmess","server":"c.example","port":443,"uuid":"u"}
proxy-groups:
  - {name: auto, type: select, proxies: ["US 001"]}
rules:
  - MATCH,auto
`

func fakeManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	t.Setenv("PROXYCORE_FAKE_CORE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	opts.BinPath = exe
	if opts.Dir == "rel" { // 用相对路径的工作目录，复现网关按 ./data/... 装配的情形
		// 先建临时目录再登记「切回原目录」：清理按后进先出执行，必须先切回去才能删掉临时目录。
		tmp := t.TempDir()
		wd, _ := os.Getwd()
		t.Cleanup(func() { _ = os.Chdir(wd) })
		if err := os.Chdir(tmp); err != nil {
			t.Fatal(err)
		}
		wd2, _ := os.Getwd()
		bin, err := filepath.Rel(wd2, exe)
		if err != nil {
			t.Skipf("test binary is on another volume: %v", err)
		}
		opts.Dir, opts.BinPath = filepath.Join("data", "proxycore"), bin
	}
	if opts.BasePort == 0 {
		opts.BasePort = 27890
	}
	opts.Logf = t.Logf
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func loads(t *testing.T, m *Manager) int {
	b, _ := os.ReadFile(filepath.Join(m.runDir, "loads"))
	return strings.Count(string(b), "\n")
}

func TestManagerLifecycle(t *testing.T) {
	var changes atomic.Int32
	m := fakeManager(t, Options{OnChange: func() { changes.Add(1) }})
	ctx := context.Background()
	if st := m.Status(); !st.Installed || st.Version != "v9.9.9" || st.Running || st.Enabled {
		t.Fatalf("fresh status: %+v", st)
	}

	// 粘贴内容添加订阅：已安装内核时顺带启动。
	v, err := m.AddSub(ctx, AddSubInput{Name: "机场", Content: clashSub})
	if err != nil || !v.HasContent || v.Format != "clash" || v.HasURL {
		t.Fatalf("add: %+v %v", v, err)
	}
	if st := m.Status(); !st.Running || !st.Enabled {
		t.Fatalf("adding a subscription must start an installed core: %+v", st)
	}
	nodes, err := m.Nodes(ctx, v.ID)
	if err != nil || len(nodes) != 3 || nodes[0].Name != "US 001" {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	if got := m.NodeURLs(); len(got) != 0 {
		t.Fatalf("nothing selected yet: %v", got)
	}

	before := loads(t, m)
	if err := m.Select(ctx, v.ID, []string{"US 001", "JP 002", "US 001"}); err != nil {
		t.Fatal(err)
	}
	if loads(t, m) != before+1 {
		t.Errorf("selecting nodes must hot-reload the running core once")
	}
	urls := m.NodeURLs()
	if len(urls) != 2 || changes.Load() == 0 {
		t.Fatalf("node urls: %v (changes=%d)", urls, changes.Load())
	}
	for _, u := range urls {
		sub, node, ok := m.NodeLabel(u)
		if !ok || sub != "机场" || (node != "US 001" && node != "JP 002") || !strings.HasPrefix(u, "http://c2a") {
			t.Errorf("label %s: %q %q %v", maskProxy(u), sub, node, ok)
		}
	}
	nodes, _ = m.Nodes(ctx, v.ID)
	sel := 0
	for _, n := range nodes {
		if n.Selected {
			sel++
			if n.Port < 27890 {
				t.Errorf("port %d below the base", n.Port)
			}
		}
	}
	if sel != 2 {
		t.Errorf("selected in node list: %d", sel)
	}

	// 已勾选的节点保留端口；再勾选同样的集合不重载。
	ports := map[string]int{}
	for _, s := range m.st.Selections {
		ports[s.Node] = s.Port
	}
	before = loads(t, m)
	if err := m.Select(ctx, v.ID, []string{"JP 002", "US 001"}); err != nil {
		t.Fatal(err)
	}
	if loads(t, m) != before {
		t.Errorf("an unchanged selection must not reload")
	}
	if err := m.Select(ctx, v.ID, []string{"US 001", "HK 003"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.st.Selections {
		if s.Node == "US 001" && s.Port != ports["US 001"] {
			t.Errorf("a kept node must keep its port")
		}
	}

	if d, err := m.Delay(ctx, v.ID, []string{"US 001"}); err != nil || d["US 001"] != 42 {
		t.Errorf("delay: %v %v", d, err)
	}

	// 停止：节点退出代理池；再启动回来。
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.Running || st.Enabled || len(m.NodeURLs()) != 0 {
		t.Fatalf("after stop: %+v urls=%v", st, m.NodeURLs())
	}
	if _, err := m.Nodes(ctx, v.ID); err == nil {
		t.Errorf("listing nodes needs a running core")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.NodeURLs()) != 2 {
		t.Errorf("after start: %v", m.NodeURLs())
	}

	// 状态落盘：重新打开后订阅、勾选、端口、代理口账号都在。
	m.Shutdown()
	m2 := fakeManager(t, Options{Dir: m.opts.Dir})
	if len(m2.Subs()) != 1 || len(m2.st.Selections) != 2 || m2.st.ListenPass != m.st.ListenPass || !m2.st.Enabled {
		t.Fatalf("reloaded state: %+v", m2.st)
	}
	go m2.Boot(ctx)
	waitFor(t, func() bool { return m2.Status().Running }, "boot must start a core that was running before")

	if err := m2.RemoveSub(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if len(m2.Subs()) != 0 || len(m2.NodeURLs()) != 0 {
		t.Errorf("remove must drop the subscription and its nodes")
	}
	if _, err := os.Stat(m2.subFile(v.ID)); !os.IsNotExist(err) {
		t.Errorf("the subscription file must be deleted")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestManagerRestartsCrashedCore(t *testing.T) {
	m := fakeManager(t, Options{})
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(m.runDir, "crash-once"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return m.Status().Restarts == 1 }, "the crash must be noticed")
	waitFor(t, func() bool { return m.Status().Running }, "a crashed core must be restarted")
	if st := m.Status(); !strings.Contains(st.LastError, "") || st.Restarts != 1 {
		t.Errorf("status: %+v", st)
	}
}

// 内核拒绝新配置时：勾选不生效（返回错误），正在运行的配置不动。
func TestManagerRejectedConfigKeepsRunningOne(t *testing.T) {
	m := fakeManager(t, Options{})
	ctx := context.Background()
	v, err := m.AddSub(ctx, AddSubInput{Name: "s", Content: clashSub})
	if err != nil {
		t.Fatal(err)
	}
	before := loads(t, m)
	err = m.Select(ctx, v.ID, []string{"FAIL_VALIDATION"})
	if err == nil || !strings.Contains(err.Error(), "内核拒绝了生成的配置") {
		t.Fatalf("want a validation error, got %v", err)
	}
	if loads(t, m) != before {
		t.Errorf("a rejected config must not be loaded")
	}
	if !m.Status().Running {
		t.Errorf("the core must keep running")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// 直连被机场拒绝（IP 鉴权）时经代理池里的代理再拉一次；错误与来源里都不出现订阅地址和代理密码。
func TestRefreshFallsBackToProxies(t *testing.T) {
	const subURL = "https://sub.example/s/secret-token?clash=1"
	var directUA string
	m := fakeManager(t, Options{
		HTTP: func(proxy string) *http.Client {
			return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				if proxy == "" {
					directUA = r.Header.Get("User-Agent")
					return respond(403, `{"error":"AUTH_FAIL，IP鉴权失败，请先登陆","code":"AUTH_FAIL"}`, nil), nil
				}
				return respond(200, clashSub, map[string]string{
					"Subscription-Userinfo":   "upload=1; download=2; total=10; expire=1793116800",
					"profile-update-interval": "12",
					"Content-Disposition":     "attachment; filename=airport.yaml",
				}), nil
			})}
		},
		FetchVia: func() []string { return []string{"http://user:hunter2@127.0.0.1:1"} },
	})
	ctx := context.Background()
	v, err := m.AddSub(ctx, AddSubInput{URL: subURL})
	if err != nil {
		t.Fatal(err)
	}
	if directUA != DefaultUA {
		t.Errorf("UA = %q", directUA)
	}
	if v.Name != "airport" || v.Via != "经代理 http://127.0.0.1:1" || v.Info == nil || v.Info.Total != 10 ||
		v.EffectiveIn != 12*60 || v.Host != "sub.example" || !v.HasContent {
		t.Fatalf("view: %+v", v)
	}

	// 全都失败：保留旧内容，错误里有机场的原话，没有订阅地址。
	m.opts.FetchVia = nil
	err = m.RefreshSub(ctx, v.ID)
	if err == nil || !strings.Contains(err.Error(), "IP鉴权失败") || !strings.Contains(err.Error(), "粘贴订阅内容") {
		t.Fatalf("refresh error: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("secrets leaked: %v", err)
	}
	after := m.view(v.ID)
	if !after.HasContent || after.LastError == "" || after.UpdatedAt != v.UpdatedAt {
		t.Errorf("a failed refresh must keep the old content: %+v", after)
	}
	raw, _ := jsonMarshalIndent(m.Subs())
	if strings.Contains(string(raw), "secret-token") {
		t.Errorf("subscription URL leaked into the view: %s", raw)
	}
}

func TestDueSubs(t *testing.T) {
	m := fakeManager(t, Options{})
	now := time.Unix(10_000_000, 0)
	day := int64(24 * 3600)
	m.st.Subs = []Sub{
		{ID: "fresh", URL: "https://a", UpdatedAt: now.Unix() - 3600},
		{ID: "stale", URL: "https://b", UpdatedAt: now.Unix() - day - 1},
		{ID: "failing", URL: "https://c", UpdatedAt: now.Unix() - 2*day, LastTryAt: now.Unix() - 60},
		{ID: "retry", URL: "https://d", UpdatedAt: now.Unix() - 2*day, LastTryAt: now.Unix() - 31*60},
		{ID: "manual", UpdatedAt: 0},
		{ID: "custom", URL: "https://e", UpdatedAt: now.Unix() - 3601, IntervalMin: 60},
		{ID: "never", URL: "https://f"},
	}
	got := strings.Join(m.dueSubs(now), ",")
	if got != "custom,never,retry,stale" {
		t.Errorf("due = %s", got)
	}
}

// 网关按 config 里的相对路径（./data/state.json → data/proxycore）装配：内核以 run/ 为工作目录启动，
// 相对路径的可执行文件和配置文件都必须先转成绝对路径（实测踩到过：fork/exec 找不到文件）。
func TestManagerRelativePaths(t *testing.T) {
	m := fakeManager(t, Options{Dir: "rel"})
	if !filepath.IsAbs(m.binPath) || !filepath.IsAbs(m.runDir) {
		t.Fatalf("paths must be absolute: %s %s", m.binPath, m.runDir)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("start with relative paths: %v", err)
	}
	if !m.Status().Running {
		t.Fatalf("not running: %+v", m.Status())
	}
}

// 订阅 ID 会拼进文件路径：格式不对的一律当作不存在，绝不碰磁盘（之前粘贴内容会先写文件再查 ID）。
func TestSubIDCannotEscapeRunDir(t *testing.T) {
	m := fakeManager(t, Options{})
	victim := filepath.Join(filepath.Dir(m.runDir), "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"../victim", `..\victim`, "../../x", "", "ABCDEF12", "abc"} {
		if err := m.SetContent(ctx, id, clashSub); err == nil {
			t.Errorf("SetContent(%q) must fail", id)
		}
		if err := m.RefreshSub(ctx, id); err == nil {
			t.Errorf("RefreshSub(%q) must fail", id)
		}
		if _, err := m.Nodes(ctx, id); err == nil {
			t.Errorf("Nodes(%q) must fail", id)
		}
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep me" {
		t.Fatalf("a file outside the subscription dir was touched: %q %v", b, err)
	}
}

// 端口释放后再分给新节点时，代理地址（用户名）跟着变：旧节点的固定设置、连通状态不会被新节点继承。
func TestReusedPortGetsNewURL(t *testing.T) {
	m := fakeManager(t, Options{})
	ctx := context.Background()
	v, err := m.AddSub(ctx, AddSubInput{Name: "s", Content: clashSub})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Select(ctx, v.ID, []string{"US 001"}); err != nil {
		t.Fatal(err)
	}
	before := m.NodeURLs()
	if err := m.Select(ctx, v.ID, []string{}); err != nil {
		t.Fatal(err)
	}
	m.Stop() // 内核停了，旧端口一定空出来
	if err := m.Select(ctx, v.ID, []string{"JP 002"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	after := m.NodeURLs()
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("urls: %v %v", before, after)
	}
	if m.st.Selections[0].Port != 27890 {
		t.Logf("port not reused in this run (%d); identity check still applies", m.st.Selections[0].Port)
	}
	if before[0] == after[0] {
		t.Errorf("a different node must get a different proxy URL even on the same port")
	}
	if _, node, ok := m.NodeLabel(before[0]); ok {
		t.Errorf("the old URL must not resolve to the new node %q", node)
	}
}

// Shutdown 之后，任何路径都不能再起新内核（否则非 Linux 平台上会留下孤儿进程）。
func TestNoStartAfterShutdown(t *testing.T) {
	m := fakeManager(t, Options{})
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m.Shutdown()
	if err := m.Restart(ctx); err == nil {
		t.Errorf("restart after shutdown must fail")
	}
	if _, err := m.AddSub(ctx, AddSubInput{Name: "s", Content: clashSub}); err != nil {
		t.Logf("add after shutdown: %v", err)
	}
	if m.Status().Running {
		t.Errorf("no core may run after shutdown")
	}
}

// 页面请求被取消（用户关了页面）时，热重载失败后的重启不能跟着失败、把内核停在那里。
func TestReloadSurvivesCancelledRequest(t *testing.T) {
	m := fakeManager(t, Options{})
	v, err := m.AddSub(context.Background(), AddSubInput{Name: "s", Content: clashSub})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = m.Select(ctx, v.ID, []string{"US 001"})
	waitFor(t, func() bool { return m.Status().Running }, "the core must still be running after a cancelled request")
}
