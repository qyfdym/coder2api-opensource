package kiroacct

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

func items(urls ...string) []kiro.ProxyItem {
	out := make([]kiro.ProxyItem, 0, len(urls))
	for _, u := range urls {
		out = append(out, kiro.ProxyItem{URL: u})
	}
	return out
}

func TestProxyStorePersistsAndDedupes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "kiro-proxies.json")
	s, err := OpenProxyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	added, existing, err := s.Add([]kiro.ProxyItem{{URL: "http://a.example:1", Note: "美国"}, {URL: "socks5://u:p@b.example:2"}})
	if err != nil || added != 2 || existing != 0 {
		t.Fatalf("add: %d %d %v", added, existing, err)
	}
	if added, existing, _ = s.Add(items("http://a.example:1", "http://c.example:3")); added != 1 || existing != 1 {
		t.Fatalf("dedupe: %d %d", added, existing)
	}

	idA := kiro.ProxyID("http://a.example:1")
	if found, err := s.SetEnabled(idA, false); !found || err != nil {
		t.Fatalf("disable: %v %v", found, err)
	}
	if got := s.EnabledURLs(); len(got) != 2 || got[0] != "socks5://u:p@b.example:2" {
		t.Errorf("enabled = %v", got)
	}
	if found, _ := s.SetEnabled("nope", true); found {
		t.Errorf("unknown id must report not found")
	}

	// 重新打开：内容、启停状态、备注都在；文件只有属主能读（里面有代理密码）。
	s2, err := OpenProxyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 3 || list[0].Enabled || list[0].Note != "美国" || !list[1].Enabled {
		t.Fatalf("reloaded = %+v", list)
	}
	if e, ok := s2.Get(list[1].ID); !ok || e.URL != "socks5://u:p@b.example:2" {
		t.Errorf("get: %+v %v", e, ok)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Errorf("stat: %v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("proxy file holds credentials, mode = %v", fi.Mode().Perm())
	}

	if found, err := s2.Remove(idA); !found || err != nil {
		t.Fatalf("remove: %v %v", found, err)
	}
	if found, _ := s2.Remove(idA); found {
		t.Errorf("second remove must report not found")
	}
	if got := len(s2.List()); got != 2 {
		t.Errorf("after remove: %d", got)
	}
}

func TestProxyStoreLimit(t *testing.T) {
	s, _ := OpenProxyStore(filepath.Join(t.TempDir(), "p.json"))
	var many []kiro.ProxyItem
	for i := 0; i < MaxManagedProxies+1; i++ {
		many = append(many, kiro.ProxyItem{URL: fmt.Sprintf("http://h%d.example:1", i)})
	}
	if _, _, err := s.Add(many); err == nil {
		t.Fatalf("exceeding the limit must fail")
	}
	if len(s.List()) != 0 {
		t.Errorf("a rejected batch must not be partially applied")
	}
}

// 写盘失败时内存里的状态不变（不会出现「界面上有、重启后没了」）。
func TestProxyStoreSaveFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 父路径是个文件：MkdirAll 必然失败。
	s, _ := OpenProxyStore(filepath.Join(blocker, "kiro-proxies.json"))
	if _, _, err := s.Add(items("http://a.example:1")); err == nil {
		t.Fatalf("expected a save error")
	}
	if len(s.List()) != 0 {
		t.Errorf("failed save must not change memory: %+v", s.List())
	}
}

func TestProxyStoreCorruptFileIsBackedUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiro-proxies.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenProxyStore(path)
	if err == nil || s == nil || len(s.List()) != 0 {
		t.Fatalf("corrupt file: store=%v err=%v", s, err)
	}
	baks, _ := filepath.Glob(path + ".bad-*")
	if len(baks) != 1 {
		t.Errorf("the corrupt file must be kept as a backup, got %v", baks)
	}
}
