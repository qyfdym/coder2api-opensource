package kiroacct

// 面板托管的 Kiro 代理池：一个 JSON 文件（与 state.json 同目录），热增删、热启停，不必改 config.json 重启。
// 文件里存的是带账号密码的完整地址（0600 权限），对外展示一律用 kiro.MaskProxyURL 脱敏。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

// MaxManagedProxies 面板托管的代理数上限。
const MaxManagedProxies = 200

// ProxyEntry 一个托管代理。
type ProxyEntry struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Note    string `json:"note,omitempty"`
	Enabled bool   `json:"enabled"`
	AddedAt int64  `json:"added_at,omitempty"`
}

type proxyFile struct {
	Proxies []ProxyEntry `json:"proxies"`
}

// ProxyStore 托管代理的持久化存储（并发安全）。
type ProxyStore struct {
	path string

	mu      sync.Mutex
	entries []ProxyEntry
}

// OpenProxyStore 打开（必要时视为空）代理存储。文件不存在 = 空；文件损坏时先备份成 .bad-<时间戳>
// 再从空开始（返回的 store 可用，error 供调用方记日志）。
func OpenProxyStore(path string) (*ProxyStore, error) {
	s := &ProxyStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("读取 %s: %w", path, err)
	}
	var f proxyFile
	if err := json.Unmarshal(data, &f); err != nil {
		bak := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
		_ = os.WriteFile(bak, data, 0o600)
		return s, fmt.Errorf("%s 不是合法 JSON（已备份为 %s，从空列表开始）: %w", path, bak, err)
	}
	seen := map[string]bool{}
	for _, e := range f.Proxies {
		n, err := kiro.NormalizeProxyURL(e.URL)
		if err != nil || n == "" || n == kiro.ProxyDirect || seen[n] {
			continue
		}
		seen[n] = true
		e.URL, e.ID = n, kiro.ProxyID(n)
		s.entries = append(s.entries, e)
	}
	return s, nil
}

// List 全部托管代理（按添加顺序，副本）。
func (s *ProxyStore) List() []ProxyEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ProxyEntry(nil), s.entries...)
}

// Get 按 ID 取一个。
func (s *ProxyStore) Get(id string) (ProxyEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID == id {
			return e, true
		}
	}
	return ProxyEntry{}, false
}

// EnabledURLs 已启用的托管代理地址（完整地址，仅供注入 Kiro 客户端）。
func (s *ProxyStore) EnabledURLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		if e.Enabled {
			out = append(out, e.URL)
		}
	}
	return out
}

// Add 追加代理（默认启用）。已存在的地址不重复添加（existing 为被跳过的个数）。
// 超过上限或落盘失败时整体不生效。
func (s *ProxyStore) Add(items []kiro.ProxyItem) (added, existing int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]ProxyEntry(nil), s.entries...)
	have := make(map[string]bool, len(next))
	for _, e := range next {
		have[e.URL] = true
	}
	now := time.Now().Unix()
	for _, it := range items {
		if have[it.URL] {
			existing++
			continue
		}
		have[it.URL] = true
		next = append(next, ProxyEntry{ID: kiro.ProxyID(it.URL), URL: it.URL, Note: it.Note, Enabled: true, AddedAt: now})
		added++
	}
	if len(next) > MaxManagedProxies {
		return 0, 0, fmt.Errorf("代理池最多 %d 个", MaxManagedProxies)
	}
	if added == 0 {
		return 0, existing, nil
	}
	if err := s.save(next); err != nil {
		return 0, 0, err
	}
	s.entries = next
	return added, existing, nil
}

// SetEnabled 启用 / 停用一个代理；ID 不存在返回 false。
func (s *ProxyStore) SetEnabled(id string, on bool) (found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]ProxyEntry(nil), s.entries...)
	for i := range next {
		if next[i].ID == id {
			next[i].Enabled = on
			if err := s.save(next); err != nil {
				return true, err
			}
			s.entries = next
			return true, nil
		}
	}
	return false, nil
}

// Remove 删除一个代理；ID 不存在返回 false。
func (s *ProxyStore) Remove(id string) (found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID != id {
			continue
		}
		next := append(append([]ProxyEntry(nil), s.entries[:i]...), s.entries[i+1:]...)
		if err := s.save(next); err != nil {
			return true, err
		}
		s.entries = next
		return true, nil
	}
	return false, nil
}

// save 原子写盘（先写临时文件再 rename）。调用方持有 s.mu。
func (s *ProxyStore) save(entries []ProxyEntry) error {
	data, err := json.MarshalIndent(proxyFile{Proxies: entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("保存代理池失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("保存代理池失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存代理池失败: %w", err)
	}
	return nil
}
