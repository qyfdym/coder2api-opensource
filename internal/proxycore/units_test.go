package proxycore

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSniff(t *testing.T) {
	links := "trojan://pw@1.2.3.4:443#US\nvmess://abc\n"
	cases := map[string]string{
		clashSub: "clash",
		links:    "links",
		base64.StdEncoding.EncodeToString([]byte(links)):    "base64",
		base64.RawURLEncoding.EncodeToString([]byte(links)): "base64",
	}
	for in, want := range cases {
		if got, err := Sniff([]byte(in)); err != nil || got != want {
			t.Errorf("Sniff(%.30q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, wantErr := range map[string]string{
		"":                                  "空的",
		"<!DOCTYPE html><html>login</html>": "网页",
		"proxy-providers:\n  a: {url: x}\n": "proxy-providers",
		"hello world":                       "认不出来",
		base64.StdEncoding.EncodeToString([]byte("just text")): "认不出来",
	} {
		if _, err := Sniff([]byte(in)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Sniff(%.30q) error = %v, want %q", in, err, wantErr)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amz-Meta-Subscription-Userinfo", "upload=1.5; download=200; total=3000; expire=1793116800; junk")
	info := ParseUserInfo(h)
	if info == nil || info.Upload != 1 || info.Download != 200 || info.Total != 3000 || info.Expire != 1793116800 {
		t.Errorf("userinfo = %+v", info)
	}
	if ParseUserInfo(http.Header{}) != nil {
		t.Errorf("no header must give nil")
	}
	for in, want := range map[string]string{
		"attachment; filename=xmrth1.net.yaml":                "xmrth1.net",
		`attachment; filename*=UTF-8''%E6%9C%BA%E5%9C%BA.yml`: "机场",
		"inline": "",
		"":       "",
	} {
		if got := dispositionName(in); got != want {
			t.Errorf("dispositionName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := errorHint([]byte(`{"error":"AUTH_FAIL","code":1}`)); got != "：AUTH_FAIL" {
		t.Errorf("hint = %q", got)
	}
	if got := errorHint([]byte(`<html><body>blocked</body></html>`)); got != "" {
		t.Errorf("html must not become a hint: %q", got)
	}
}

func TestCleanURL(t *testing.T) {
	if u, err := CleanURL("  https://a.example/s?x=1 "); err != nil || u != "https://a.example/s?x=1" {
		t.Errorf("%q %v", u, err)
	}
	for _, bad := range []string{"", "ftp://a/b", "a.example/s", "https://"} {
		if _, err := CleanURL(bad); err == nil {
			t.Errorf("CleanURL(%q) must fail", bad)
		}
	}
}

func TestBuildConfig(t *testing.T) {
	sels := []Selection{
		{SubID: "s1", Node: "🇺🇸 美国 [IPLC]|01 (x1.0) $+? `bt`", Port: 17890},
		{SubID: "s1", Node: "🇺🇸 美国 [IPLC]|01 (x1.0) $+? `bt`", Port: 17899}, // 重复：只取第一条
		{SubID: "gone", Node: "JP 1", Port: 17891},                          // 订阅没内容：只有守卫
	}
	raw, err := buildConfig(buildInput{Controller: "127.0.0.1:1", Secret: "s", User: "u", Pass: "p", Subs: []string{"s1"}, Selections: sels})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Rules     []string                  `json:"rules"`
		AllowLan  bool                      `json:"allow-lan"`
		Providers map[string]map[string]any `json:"proxy-providers"`
		Groups    []struct {
			Name   string   `json:"name"`
			Type   string   `json:"type"`
			Use    []string `json:"use"`
			Filter string   `json:"filter"`
		} `json:"proxy-groups"`
		Listeners []struct {
			Port   int    `json:"port"`
			Listen string `json:"listen"`
			Proxy  string `json:"proxy"`
			Users  []struct {
				Username, Password string
			} `json:"users"`
		} `json:"listeners"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0] != "MATCH,REJECT" || cfg.AllowLan {
		t.Errorf("base: %+v", cfg)
	}
	if _, ok := cfg.Providers["sub-gone"]; ok || cfg.Providers["sub-s1"]["path"] != "./subs/s1.txt" || cfg.Providers[guardProvider] == nil {
		t.Errorf("providers: %v", cfg.Providers)
	}
	if len(cfg.Groups) != 2 || len(cfg.Listeners) != 2 {
		t.Fatalf("groups=%d listeners=%d", len(cfg.Groups), len(cfg.Listeners))
	}
	g := cfg.Groups[0]
	if g.Type != "select" || len(g.Use) != 2 || g.Use[0] != "sub-s1" || g.Use[1] != guardProvider {
		t.Errorf("group: %+v (the node provider must come before the guard)", g)
	}
	if strings.Contains(g.Filter, "`") {
		t.Errorf("a backtick would split mihomo's filter: %s", g.Filter)
	}
	// 用 Go 的正则验证（mihomo 的 regexp2 对这些转义的解释相同；反引号 \x60 两边都支持）。
	re := regexp.MustCompile(g.Filter)
	if !re.MatchString(sels[0].Node) || !re.MatchString(guardName) || re.MatchString("🇺🇸 美国 [IPLC]|02 (x1.0) $+? `bt`") || re.MatchString("US") {
		t.Errorf("filter %s must match exactly the node and the guard", g.Filter)
	}
	if missing := cfg.Groups[1]; len(missing.Use) != 1 || missing.Use[0] != guardProvider {
		t.Errorf("a node of a subscription without content must only have the guard: %+v", missing)
	}
	l := cfg.Listeners[0]
	if l.Port != 17890 || l.Listen != "127.0.0.1" || l.Proxy != g.Name || len(l.Users) != 1 ||
		l.Users[0].Username != listenUser("u", sels[0]) || l.Users[0].Password != "p" {
		t.Errorf("listener: %+v", l)
	}
	if cfg.Listeners[1].Users[0].Username == l.Users[0].Username {
		t.Errorf("each node listener needs its own username")
	}
	if !strings.Contains(string(raw), `"exclude-type": "Direct|`) {
		t.Errorf("direct-type nodes must be excluded from node groups")
	}
}

func TestAssetName(t *testing.T) {
	for in, want := range map[string]string{
		"linux/amd64":   "mihomo-linux-amd64-compatible-v1.2.3.gz",
		"linux/arm64":   "mihomo-linux-arm64-v1.2.3.gz",
		"windows/amd64": "mihomo-windows-amd64-compatible-v1.2.3.zip",
		"darwin/arm64":  "mihomo-darwin-arm64-v1.2.3.gz",
	} {
		os, arch, _ := strings.Cut(in, "/")
		if got, err := AssetName(os, arch, "v1.2.3"); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := AssetName("plan9", "amd64", "v1"); err == nil {
		t.Errorf("unknown platform must fail")
	}
	if m, err := NormalizeMirror(" https://ghfast.top "); err != nil || m != "https://ghfast.top/" {
		t.Errorf("mirror: %q %v", m, err)
	}
	if _, err := NormalizeMirror("http://insecure.example/"); err == nil {
		t.Errorf("a plain-http mirror must be refused (we execute what it serves)")
	}
}

func TestExtractBinary(t *testing.T) {
	payload := append([]byte("\x7fELF"), bytes.Repeat([]byte{1}, 100)...)
	dir := t.TempDir()

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(payload)
	_ = w.Close()

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("README.md")
	_, _ = f.Write([]byte("readme"))
	f, _ = zw.Create("mihomo-windows-amd64.exe")
	_, _ = f.Write(payload)
	_ = zw.Close()

	for name, data := range map[string][]byte{"gz": gz.Bytes(), "zip": zb.Bytes(), "raw": payload} {
		dest := filepath.Join(dir, name)
		if err := extractBinary(data, dest); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got, _ := os.ReadFile(dest); !bytes.Equal(got, payload) {
			t.Errorf("%s: wrong content", name)
		}
	}
	if err := extractBinary([]byte("not a binary at all"), filepath.Join(dir, "x")); err == nil {
		t.Errorf("garbage must be refused")
	}
}
