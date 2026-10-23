// kiro_proxies.go 面板「代理池」页的后端：Kiro 出站代理池的增删、启停、连通性测试。
//
// Kiro 按出口 IP 的地区放出模型（国内 IP 没有 Claude），所以需要海外代理。池里的成员存在
// state.json 同目录的 kiro-proxies.json 里，改动立即生效，不需要改 config.json 或重启。
// config.json 里的 kiro.proxy_pool 仍然有效，在这里只读展示。
// 代理地址带账号密码，下发给前端和写日志的一律是脱敏后的 scheme://host:port。
package panel

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
)

// kiroProxiesMaxBody 粘贴文本上限（200 行代理远小于这个值）。
const kiroProxiesMaxBody = 256 << 10

// kiroProxiesReady 需要 Kiro 启用且代理池存储就绪。
func (p *Panel) kiroProxiesReady(w http.ResponseWriter) bool {
	if !p.kiroEnabled(w) {
		return false
	}
	if p.cfg.Kiro.Proxies == nil {
		writeErr(w, http.StatusNotImplemented, "代理池存储未初始化")
		return false
	}
	return true
}

// kiroAuths 池里全部 Kiro 账号（含已禁用的）。
func (p *Panel) kiroAuths() []*auth.Auth {
	var out []*auth.Auth
	for _, st := range p.cfg.Pool.List() {
		if st.Platform != auth.PlatformKiro {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.IsKiro() {
			out = append(out, a)
		}
	}
	return out
}

// kiroResync 执行一次代理池改动并让它生效：apply 改存储，成功后把启用的成员热更新进 Kiro 客户端，
// 再对「出口因此变了」的账号作废其缓存的模型目录与「模型不可用」冷却（目录按出口地区过滤，
// 旧出口拉到的目录与冷却不再适用）。返回出口变了的账号数。
func (p *Panel) kiroResync(apply func() error) (moved int, err error) {
	client := p.cfg.Kiro.Client
	accts := p.kiroAuths()
	before := make(map[string]string, len(accts))
	for _, a := range accts {
		before[a.UID] = client.EgressKey(kiroacct.Cred(a))
	}
	if err := apply(); err != nil {
		return 0, err
	}
	client.SetManagedProxies(p.cfg.Kiro.Proxies.EnabledURLs())
	for _, a := range accts {
		if client.EgressKey(kiroacct.Cred(a)) == before[a.UID] {
			continue
		}
		client.InvalidateCatalog(a.UID)
		p.cfg.Pool.ClearModelCooldowns(a.UID)
		moved++
	}
	return moved, nil
}

// kiroProxiesList GET：池里的全部代理（配置文件里的只读 + 面板托管的）、每个代理上跑着几个账号、
// 各账号当前的出口。
func (p *Panel) kiroProxiesList(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	client := p.cfg.Kiro.Client
	members := client.PoolMembers()
	inPool := make(map[string]kiro.PoolMember, len(members))
	for _, m := range members {
		inPool[m.URL] = m
	}

	// 每个代理上跑着的账号数；顺便列出各账号当前的出口。
	users := map[string]int{}
	accounts := []map[string]any{}
	for _, a := range p.kiroAuths() {
		cred := kiroacct.Cred(a)
		if key := client.EgressKey(cred); key != "" {
			users[key]++
		}
		info := client.ProxyInfo(cred)
		accounts = append(accounts, map[string]any{
			"uid": a.UID, "nickname": a.Nickname, "kind": info.Kind, "proxy": info.Label, "down": info.Down,
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i]["uid"].(string) < accounts[j]["uid"].(string) })

	row := func(id, rawURL, note, source string, enabled bool) map[string]any {
		m, active := inPool[rawURL]
		e := map[string]any{
			"id": id, "proxy": kiro.MaskProxyURL(rawURL), "auth": hasProxyAuth(rawURL), "note": note,
			"source": source, "enabled": enabled, "active": active, "down": active && m.Down, "accounts": users[rawURL],
		}
		if t, ok := p.krProxyTests.Load(id); ok {
			e["test"] = t
		}
		return e
	}
	rows := []map[string]any{}
	for _, m := range members {
		if m.Config {
			rows = append(rows, row(kiro.ProxyID(m.URL), m.URL, "", "config", true))
		}
	}
	for _, e := range p.cfg.Kiro.Proxies.List() {
		rows = append(rows, row(e.ID, e.URL, e.Note, "panel", e.Enabled))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxies": rows, "accounts": accounts, "pool_active": len(members) > 0, "max": kiroacct.MaxManagedProxies,
	})
}

func hasProxyAuth(raw string) bool {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	return strings.Contains(rest, "@")
}

// kiroProxiesAdd POST {text, scheme}：一次粘贴多行（写法见 kiro.ParseProxyList）。
func (p *Panel) kiroProxiesAdd(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	var req struct {
		Text   string `json:"text"`
		Scheme string `json:"scheme"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, kiroProxiesMaxBody)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	items, skipped := kiro.ParseProxyList(req.Text, req.Scheme)
	if len(items) == 0 {
		msg := "没有可添加的代理"
		if len(skipped) > 0 {
			msg += "：" + strings.Join(skipped, "；")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg, "skipped": skipped})
		return
	}
	// 配置文件里已有的地址不再重复托管（池里去重后本来就只有一份）。
	inConfig := map[string]bool{}
	for _, m := range p.cfg.Kiro.Client.PoolMembers() {
		if m.Config {
			inConfig[m.URL] = true
		}
	}
	fresh := items[:0:0]
	configDup := 0
	for _, it := range items {
		if inConfig[it.URL] {
			configDup++
			continue
		}
		fresh = append(fresh, it)
	}
	var added, existing int
	moved, err := p.kiroResync(func() (e error) {
		added, existing, e = p.cfg.Kiro.Proxies.Add(fresh)
		return e
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ids := make([]string, 0, len(fresh))
	for _, it := range fresh {
		ids = append(ids, kiro.ProxyID(it.URL))
	}
	log.Printf("panel: kiro proxies add: %d added, %d existing, %d skipped, %d account(s) moved", added, existing+configDup, len(skipped), moved)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "existing": existing + configDup, "skipped": skipped, "moved": moved, "ids": ids,
	})
}

// kiroProxiesEnable POST {enabled}：启用 / 停用一个托管代理。
func (p *Panel) kiroProxiesEnable(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := readJSONBody(r, &req); err != nil || req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "请求体需要 {\"enabled\": true|false}")
		return
	}
	id := r.PathValue("id")
	found := false
	moved, err := p.kiroResync(func() (e error) {
		found, e = p.cfg.Kiro.Proxies.SetEnabled(id, *req.Enabled)
		return e
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "代理不存在（配置文件里的代理不能在这里停用）")
		return
	}
	log.Printf("panel: kiro proxy %s enabled=%v, %d account(s) moved", id, *req.Enabled, moved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved})
}

// kiroProxiesRemove POST：删除一个托管代理。
func (p *Panel) kiroProxiesRemove(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	id := r.PathValue("id")
	found := false
	moved, err := p.kiroResync(func() (e error) {
		found, e = p.cfg.Kiro.Proxies.Remove(id)
		return e
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "代理不存在（配置文件里的代理要改 config.json）")
		return
	}
	p.krProxyTests.Delete(id)
	log.Printf("panel: kiro proxy %s removed, %d account(s) moved", id, moved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved})
}

// kiroProxiesTest POST：经这个代理探测一次（能否连到 Kiro、出口 IP 在哪个国家）。停用的代理也能测。
func (p *Panel) kiroProxiesTest(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	id := r.PathValue("id")
	target := ""
	if e, ok := p.cfg.Kiro.Proxies.Get(id); ok {
		target = e.URL
	} else {
		for _, m := range p.cfg.Kiro.Client.PoolMembers() {
			if m.Config && kiro.ProxyID(m.URL) == id {
				target = m.URL
			}
		}
	}
	if target == "" {
		writeErr(w, http.StatusNotFound, "代理不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := p.cfg.Kiro.Client.TestProxy(ctx, target)
	p.krProxyTests.Store(id, res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "test": res})
}
