// proxycore.go 面板「代理池」页的机场订阅与代理内核（mihomo）部分，以及 WorkBuddy / Qoder 的出口设置。
//
// 订阅地址等同密码：只在服务端使用，下发给前端的只有主机名。勾选的节点由内核在 127.0.0.1 上开带认证的
// 代理口，作为代理池成员参与分配；它们的地址同样只以脱敏形式（scheme://host:port）出现在页面上。
// 安装 / 上传内核等于在服务器上运行一个程序，所以面板没设 api_key 时拒绝。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiroacct"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxycore"
)

// subBodyMax 带订阅内容的请求体上限。通用的 readJSONBody 只读 64 KB，几十个节点的 Clash 配置就超了。
const subBodyMax = 9 << 20

func readBigJSON(w http.ResponseWriter, r *http.Request, v any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, subBodyMax))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// coreReady 需要 Kiro（代理池在 Kiro 客户端里）与代理内核管理器。
func (p *Panel) coreReady(w http.ResponseWriter) bool {
	if !p.kiroEnabled(w) {
		return false
	}
	if p.cfg.ProxyCore == nil {
		writeErr(w, http.StatusNotImplemented, "代理内核管理未启用")
		return false
	}
	return true
}

// requireAPIKey 会在服务器上运行程序（安装 / 上传 / 启动内核）、让服务器去拉任意地址（添加 / 更新订阅）、
// 或改动内核配置（粘贴内容 / 勾选节点）的操作，要求面板设了 api_key。没设 api_key 时谁都能打开面板，
// 这些操作等于把服务器借给任何人用。停止内核、删除订阅这类收缩操作不受限制。
func (p *Panel) requireAPIKey(w http.ResponseWriter) bool {
	if strings.TrimSpace(p.apiKey()) == "" {
		writeErr(w, http.StatusForbidden, "面板没有设置 api_key：任何人都能打开面板，这时不允许安装 / 运行代理内核或添加订阅。请先在配置页设置 api_key")
		return false
	}
	return true
}

// subID 路径里的订阅 ID（格式不对直接 404：ID 会拼进文件路径）。
func subID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !proxycore.ValidID(id) {
		writeErr(w, http.StatusNotFound, "订阅不存在")
		return "", false
	}
	return id, true
}

// coreGet GET：内核状态 + 订阅列表。
func (p *Panel) coreGet(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": p.cfg.ProxyCore.Status(), "subs": p.cfg.ProxyCore.Subs(),
		"max_subs": proxycore.MaxSubs, "max_selections": proxycore.MaxSelections,
	})
}

func (p *Panel) coreLogs(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": p.cfg.ProxyCore.Logs(200)})
}

// coreInstall POST {mirror}：后台下载最新版内核，进度看 GET /proxycore 的 status.install。
func (p *Panel) coreInstall(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	var req struct {
		Mirror string `json:"mirror"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if err := p.cfg.ProxyCore.StartInstall(req.Mirror); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: proxycore install started (mirror=%v)", strings.TrimSpace(req.Mirror) != "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// coreUpload POST（请求体 = 文件内容）：安装上传的内核（.gz / .zip / 可执行文件）。
func (p *Panel) coreUpload(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 151<<20)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var ver string
	_, err := p.kiroResync(func() (e error) {
		ver, e = p.cfg.ProxyCore.InstallFrom(ctx, r.Body)
		return e
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: proxycore uploaded and installed %s", ver)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": ver})
}

// coreAction POST start / stop / restart。
func (p *Panel) coreAction(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	action := r.PathValue("action")
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	var op func() error
	switch action {
	case "start":
		op = func() error { return p.cfg.ProxyCore.Start(ctx) }
	case "restart":
		op = func() error { return p.cfg.ProxyCore.Restart(ctx) }
	case "stop":
		op = p.cfg.ProxyCore.Stop
	default:
		writeErr(w, http.StatusNotFound, "未知操作")
		return
	}
	if action != "stop" && !p.requireAPIKey(w) {
		return
	}
	moved, err := p.kiroResync(op)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: proxycore %s, %d account(s) moved", action, moved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved, "status": p.cfg.ProxyCore.Status()})
}

// subAdd POST {name, url, ua, content}：添加订阅（给了 content 就直接用，否则立刻拉一次）。
// 拉取失败时订阅仍会保留（带着错误），可以稍后重试或粘贴内容。
func (p *Panel) subAdd(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		UA      string `json:"ua"`
		Content string `json:"content"`
	}
	if err := readBigJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var (
		view   proxycore.SubView
		addErr error
	)
	moved, _ := p.kiroResync(func() error {
		view, addErr = p.cfg.ProxyCore.AddSub(ctx, proxycore.AddSubInput{Name: req.Name, URL: req.URL, UA: req.UA, Content: req.Content})
		return nil
	})
	if view.ID == "" { // 没保存下来：地址不对、重复、超上限、内容认不出来……
		msg := "添加失败"
		if addErr != nil {
			msg = addErr.Error()
		}
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	resp := map[string]any{"ok": true, "sub": view, "moved": moved}
	if addErr != nil { // 订阅已保存，但拉取 / 加载失败：可以稍后重试或粘贴内容
		resp["warning"] = addErr.Error()
	}
	log.Printf("panel: proxycore sub added id=%s host=%s content=%v warning=%v", view.ID, view.Host, view.HasContent, addErr != nil)
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) subRefresh(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	_, err := p.kiroResync(func() error { return p.cfg.ProxyCore.RefreshSub(ctx, id) })
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) subContent(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := readBigJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if _, err := p.kiroResync(func() error { return p.cfg.ProxyCore.SetContent(ctx, id, req.Content) }); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) subUpdate(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        string  `json:"name"`
		URL         *string `json:"url"`
		UA          string  `json:"ua"`
		IntervalMin int     `json:"interval_min"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if err := p.cfg.ProxyCore.UpdateSub(id, req.Name, req.URL, req.UA, req.IntervalMin); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) subRemove(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	moved, err := p.kiroResync(func() error { return p.cfg.ProxyCore.RemoveSub(ctx, id) })
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved})
}

func (p *Panel) subNodes(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	nodes, err := p.cfg.ProxyCore.Nodes(ctx, id)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

// subSelect POST {nodes: [...]}：设置这个订阅下勾选的节点（整体替换）。
func (p *Panel) subSelect(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) || !p.requireAPIKey(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := readBigJSON(w, r, &req); err != nil || req.Nodes == nil {
		writeErr(w, http.StatusBadRequest, "请求体需要 {\"nodes\": [...]}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	moved, err := p.kiroResync(func() error { return p.cfg.ProxyCore.Select(ctx, id, req.Nodes) })
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: proxycore sub %s: %d node(s) selected, %d account(s) moved", id, len(req.Nodes), moved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moved": moved})
}

func (p *Panel) subDelay(w http.ResponseWriter, r *http.Request) {
	if !p.coreReady(w) {
		return
	}
	id, ok := subID(w, r)
	if !ok {
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := readBigJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	d, err := p.cfg.ProxyCore.Delay(ctx, id, req.Nodes)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delay": d})
}

// ---------------------------------------------------------------------------
// WorkBuddy / Qoder 出口
// ---------------------------------------------------------------------------

// egressRows 两个平台当前的出口设置与实际出口（脱敏）。
func (p *Panel) egressRows() []map[string]any {
	k := p.cfg.Kiro
	if k == nil || k.Proxies == nil {
		return nil
	}
	rows := []map[string]any{}
	for _, plat := range []string{egress.WorkBuddy, egress.Qoder} {
		pe := k.Proxies.Platform(plat)
		row := map[string]any{"platform": plat, "name": kiroacct.PlatformName(plat), "mode": pe.Mode, "proxy_id": pe.ProxyID, "direct_cn": pe.DirectCN}
		switch raw, err := k.PlatformProxy(plat, "example.com"); {
		case err != nil:
			row["error"] = err.Error()
		case raw != "":
			row["current"] = kiro.MaskProxyURL(raw)
			row["current_label"] = p.proxyLabel(raw)
		}
		rows = append(rows, row)
	}
	return rows
}

// proxyLabel 池成员的说明：订阅节点给出「订阅名 · 节点名」，其它为空。
func (p *Panel) proxyLabel(raw string) string {
	if p.cfg.ProxyCore == nil {
		return ""
	}
	if sub, node, ok := p.cfg.ProxyCore.NodeLabel(raw); ok {
		return sub + " · " + node
	}
	return ""
}

// egressSet POST {platform, mode: ""|"auto"|"pin", proxy_id, direct_cn}：设置 WorkBuddy / Qoder 的出口。
func (p *Panel) egressSet(w http.ResponseWriter, r *http.Request) {
	if !p.kiroProxiesReady(w) {
		return
	}
	var req struct {
		Platform string `json:"platform"`
		Mode     string `json:"mode"`
		ProxyID  string `json:"proxy_id"`
		DirectCN *bool  `json:"direct_cn"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if req.Mode == "off" {
		req.Mode = kiroacct.EgressOff
	}
	pe := kiroacct.PlatformEgress{Mode: req.Mode, ProxyID: req.ProxyID, DirectCN: true}
	if req.DirectCN != nil {
		pe.DirectCN = *req.DirectCN
	}
	if pe.Mode == kiroacct.EgressPin {
		found := false
		for _, m := range p.cfg.Kiro.Client.PoolMembers() {
			found = found || kiro.ProxyID(m.URL) == pe.ProxyID
		}
		if !found {
			writeErr(w, http.StatusBadRequest, "选的代理不在代理池里")
			return
		}
	}
	if err := p.cfg.Kiro.Proxies.SetPlatform(req.Platform, pe); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: egress %s -> mode=%q direct_cn=%v", req.Platform, pe.Mode, pe.DirectCN)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "platforms": p.egressRows()})
}
