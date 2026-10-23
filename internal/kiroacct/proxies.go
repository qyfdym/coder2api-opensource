package kiroacct

// 面板托管的代理池（Kiro 按账号分配，WorkBuddy / Qoder 可选用）：一个 JSON 文件（与 state.json 同目录），热增删、热启停，不必改 config.json 重启。
// 文件里存的是带账号密码的完整地址（0600 权限），对外展示一律用 kiro.MaskProxyURL 脱敏。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
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

// 平台出口模式（WorkBuddy / Qoder）。
const (
	EgressOff  = ""     // 关闭：跟随环境变量 HTTPS_PROXY（缺省，与以前完全一样）
	EgressAuto = "auto" // 代理池里稳定选一个，连不上顺延
	EgressPin  = "pin"  // 固定用池里的某一个（ProxyID）
)

// PlatformEgress 一个平台（WorkBuddy / Qoder）的出口设置。
type PlatformEgress struct {
	Mode string `json:"mode,omitempty"`
	// ProxyID Mode=pin 时固定使用的池成员（kiro.ProxyID）。
	ProxyID string `json:"proxy_id,omitempty"`
	// DirectCN 国内域名（*.cn、*.tencent.com、*.qq.com）不走代理。国内版上游对海外出口不友好
	// （copilot.tencent.com 会直接复位海外来的 TLS 连接），缺省打开。
	DirectCN bool `json:"direct_cn"`
}

type proxyFile struct {
	Proxies   []ProxyEntry              `json:"proxies"`
	Platforms map[string]PlatformEgress `json:"platforms,omitempty"`
}

// ProxyStore 托管代理的持久化存储（并发安全）。
type ProxyStore struct {
	path string

	mu        sync.Mutex
	entries   []ProxyEntry
	platforms map[string]PlatformEgress
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
	for name, pe := range f.Platforms {
		if validPlatform(name) && validEgressMode(pe.Mode) {
			if s.platforms == nil {
				s.platforms = map[string]PlatformEgress{}
			}
			s.platforms[name] = pe
		}
	}
	return s, nil
}

func validPlatform(name string) bool { return name == egress.WorkBuddy || name == egress.Qoder }

func validEgressMode(m string) bool { return m == EgressOff || m == EgressAuto || m == EgressPin }

// Platform 某个平台当前的出口设置（没设置过 = 关闭、国内域名直连）。
func (s *ProxyStore) Platform(name string) PlatformEgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pe, ok := s.platforms[name]; ok {
		return pe
	}
	return PlatformEgress{DirectCN: true}
}

// SetPlatform 修改某个平台的出口设置并落盘。
func (s *ProxyStore) SetPlatform(name string, pe PlatformEgress) error {
	if !validPlatform(name) {
		return fmt.Errorf("未知平台 %q", name)
	}
	if !validEgressMode(pe.Mode) {
		return fmt.Errorf("未知出口模式 %q（off / auto / pin）", pe.Mode)
	}
	if pe.Mode == EgressPin && pe.ProxyID == "" {
		return errors.New("固定代理需要选一个代理")
	}
	if pe.Mode != EgressPin {
		pe.ProxyID = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]PlatformEgress, len(s.platforms)+1)
	for k, v := range s.platforms {
		next[k] = v
	}
	next[name] = pe
	if err := s.saveLocked(s.entries, next); err != nil {
		return err
	}
	s.platforms = next
	return nil
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
func (s *ProxyStore) save(entries []ProxyEntry) error { return s.saveLocked(entries, s.platforms) }

func (s *ProxyStore) saveLocked(entries []ProxyEntry, platforms map[string]PlatformEgress) error {
	data, err := json.MarshalIndent(proxyFile{Proxies: entries, Platforms: platforms}, "", "  ")
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
