package proxycore

// 订阅的增删改、更新（含定时更新）、节点列表、勾选与测速。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

func jsonUnmarshal(b []byte, v any) error     { return json.Unmarshal(b, v) }
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// SubView 订阅的展示信息（不含订阅地址）。
type SubView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Host        string    `json:"host,omitempty"`
	HasURL      bool      `json:"has_url"`
	UA          string    `json:"ua,omitempty"`
	IntervalMin int       `json:"interval_min"`
	EffectiveIn int       `json:"effective_interval_min"`
	UpdatedAt   int64     `json:"updated_at,omitempty"`
	LastTryAt   int64     `json:"last_try_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Via         string    `json:"via,omitempty"`
	Format      string    `json:"format,omitempty"`
	Size        int       `json:"size,omitempty"`
	Info        *UserInfo `json:"info,omitempty"`
	Home        string    `json:"home,omitempty"`
	HasContent  bool      `json:"has_content"`
	Selected    int       `json:"selected"`
	Refreshing  bool      `json:"refreshing,omitempty"`
}

// Subs 全部订阅（按添加顺序）。
func (m *Manager) Subs() []SubView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SubView, 0, len(m.st.Subs))
	for _, s := range m.st.Subs {
		v := SubView{ID: s.ID, Name: s.Name, Host: HostOf(s.URL), HasURL: s.URL != "", UA: s.UA,
			IntervalMin: s.IntervalMin, EffectiveIn: s.effectiveInterval(), UpdatedAt: s.UpdatedAt, LastTryAt: s.LastTryAt,
			LastError: s.LastError, Via: s.Via, Format: s.Format, Size: s.Size, Info: s.Info, Home: s.Home,
			Refreshing: m.refreshing[s.ID]}
		if fi, err := statFile(m.subFile(s.ID)); err == nil && fi > 0 {
			v.HasContent = true
		}
		for _, sel := range m.st.Selections {
			if sel.SubID == s.ID {
				v.Selected++
			}
		}
		out = append(out, v)
	}
	return out
}

// idRe 订阅 ID 的格式（randHex(4) 生成）。ID 会拼进文件路径，外部传入的必须先过这一关。
var idRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// ValidID 订阅 ID 格式是否合法。
func ValidID(id string) bool { return idRe.MatchString(id) }

var errNoSub = errors.New("订阅不存在")

func (m *Manager) findLocked(id string) int {
	if !ValidID(id) {
		return -1
	}
	for i, s := range m.st.Subs {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// AddSubInput 添加订阅的参数：URL 与 Content 至少给一个（只给 Content = 手动维护，不自动更新）。
type AddSubInput struct {
	Name    string
	URL     string
	UA      string
	Content string
}

// AddSub 添加订阅：给了内容就直接用，否则立刻拉一次。拉取失败时订阅仍然保留（带着错误），
// 可以稍后重试或粘贴内容。内核已安装且还没启动时顺带启动。
func (m *Manager) AddSub(ctx context.Context, in AddSubInput) (SubView, error) {
	in.Name, in.UA = strings.TrimSpace(in.Name), strings.TrimSpace(in.UA)
	content := strings.TrimSpace(in.Content)
	var rawURL string
	if strings.TrimSpace(in.URL) != "" {
		u, err := CleanURL(in.URL)
		if err != nil {
			return SubView{}, err
		}
		rawURL = u
	}
	if rawURL == "" && content == "" {
		return SubView{}, errors.New("请填订阅地址，或者粘贴订阅内容")
	}
	var format string
	if content != "" {
		f, err := Sniff([]byte(content))
		if err != nil {
			return SubView{}, err
		}
		format = f
	}
	m.mu.Lock()
	if len(m.st.Subs) >= MaxSubs {
		m.mu.Unlock()
		return SubView{}, fmt.Errorf("最多 %d 个订阅", MaxSubs)
	}
	for _, s := range m.st.Subs {
		if rawURL != "" && s.URL == rawURL {
			m.mu.Unlock()
			return SubView{}, fmt.Errorf("这个订阅已经添加过了（%s）", s.Name)
		}
	}
	now := time.Now().Unix()
	s := Sub{ID: randHex(4), Name: in.Name, URL: rawURL, UA: in.UA, AddedAt: now}
	if s.Name == "" {
		s.Name = HostOf(rawURL)
		if s.Name == "" {
			s.Name = "订阅 " + s.ID
		}
	}
	m.st.Subs = append(m.st.Subs, s)
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return SubView{}, err
	}
	if content != "" {
		if err := m.storeContent(ctx, s.ID, []byte(content), format, "手动粘贴", nil); err != nil {
			return m.view(s.ID), err
		}
	} else if err := m.RefreshSub(ctx, s.ID); err != nil {
		m.autoStart(ctx)
		return m.view(s.ID), err
	}
	m.autoStart(ctx)
	return m.view(s.ID), nil
}

// autoStart 内核已安装、还没被启动过、又有了订阅时，顺带启动（用户加订阅显然就是要用）。
func (m *Manager) autoStart(ctx context.Context) {
	m.mu.Lock()
	enabled := m.st.Enabled
	m.mu.Unlock()
	if enabled || !m.Installed() {
		return
	}
	_ = m.Start(ctx) // 失败原因已由 startLocked 记录在状态里
}

func (m *Manager) view(id string) SubView {
	for _, v := range m.Subs() {
		if v.ID == id {
			return v
		}
	}
	return SubView{ID: id}
}

// UpdateSub 修改订阅的名称 / 地址 / User-Agent / 更新间隔（url 为 nil = 不改地址）。
func (m *Manager) UpdateSub(id, name string, rawURL *string, ua string, intervalMin int) error {
	var cleaned string
	if rawURL != nil && strings.TrimSpace(*rawURL) != "" {
		u, err := CleanURL(*rawURL)
		if err != nil {
			return err
		}
		cleaned = u
	}
	if intervalMin != 0 && intervalMin < minIntervalMin {
		return fmt.Errorf("更新间隔最少 %d 分钟", minIntervalMin)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findLocked(id)
	if i < 0 {
		return errors.New("订阅不存在")
	}
	if n := strings.TrimSpace(name); n != "" {
		m.st.Subs[i].Name = n
	}
	if rawURL != nil {
		m.st.Subs[i].URL = cleaned
	}
	m.st.Subs[i].UA = strings.TrimSpace(ua)
	m.st.Subs[i].IntervalMin = intervalMin
	return m.saveLocked()
}

// RemoveSub 删除订阅（连同它下面勾选的节点）。
func (m *Manager) RemoveSub(ctx context.Context, id string) error {
	m.op.Lock()
	m.mu.Lock()
	i := m.findLocked(id)
	if i < 0 {
		m.mu.Unlock()
		m.op.Unlock()
		return errors.New("订阅不存在")
	}
	m.st.Subs = append(m.st.Subs[:i:i], m.st.Subs[i+1:]...)
	kept := m.st.Selections[:0:0]
	removed := 0
	for _, s := range m.st.Selections {
		if s.SubID == id {
			removed++
			continue
		}
		kept = append(kept, s)
	}
	m.st.Selections = kept
	err := m.saveLocked()
	m.mu.Unlock()
	if err == nil {
		_ = removeFile(m.subFile(id))
		err = m.applyLocked(ctx)
	}
	m.op.Unlock()
	if removed > 0 {
		m.changed()
	}
	return err
}

// RefreshSub 立即更新一次订阅：先直连，失败再经代理池里的代理（优先订阅节点）。
// 内容有变化才重载内核。
func (m *Manager) RefreshSub(ctx context.Context, id string) error {
	m.mu.Lock()
	i := m.findLocked(id)
	if i < 0 {
		m.mu.Unlock()
		return errors.New("订阅不存在")
	}
	s := m.st.Subs[i]
	if s.URL == "" {
		m.mu.Unlock()
		return errors.New("这个订阅没有地址（内容是手动粘贴的），请重新粘贴内容")
	}
	if m.refreshing[id] {
		m.mu.Unlock()
		return errors.New("正在更新，请稍候")
	}
	m.refreshing[id] = true
	m.st.Subs[i].LastTryAt = time.Now().Unix()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.refreshing, id)
		m.mu.Unlock()
	}()

	res, via, err := m.fetchSub(ctx, s)
	if err != nil {
		m.mu.Lock()
		if j := m.findLocked(id); j >= 0 {
			m.st.Subs[j].LastError = err.Error()
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		return err
	}
	return m.storeContent(ctx, id, res.Body, res.Format, via, &res)
}

func (m *Manager) fetchSub(ctx context.Context, s Sub) (FetchResult, string, error) {
	res, err := Fetch(ctx, m.httpClient(""), s.URL, s.UA)
	if err == nil {
		return res, "直连", nil
	}
	errs := []string{"直连：" + err.Error()}
	if m.opts.FetchVia != nil {
		// 明文 http 的订阅地址（含令牌）经第三方代理会被代理看到，只经订阅自己的节点重试。
		plain := false
		if u, perr := url.Parse(s.URL); perr == nil && u.Scheme == "http" {
			plain = true
		}
		tried := 0
		for _, p := range m.opts.FetchVia() {
			if tried >= maxFetchVia || ctx.Err() != nil {
				break
			}
			if plain && !m.IsNodeURL(p) {
				continue
			}
			tried++
			r2, e2 := Fetch(ctx, m.httpClient(p), s.URL, s.UA)
			if e2 == nil {
				return r2, "经代理 " + maskProxy(p), nil
			}
			errs = append(errs, "经代理 "+maskProxy(p)+"："+e2.Error())
		}
	}
	msg := strings.Join(errs, "；")
	if len(errs) == 1 && strings.Contains(msg, "HTTP 403") {
		msg += "（机场可能不让这台服务器的 IP 直接拉订阅：可以先粘贴订阅内容，勾选节点后，之后的更新会自动经这些节点拉取）"
	}
	return FetchResult{}, "", errors.New(msg)
}

// SetContent 手动粘贴订阅内容（机场不让服务器直接拉订阅时用）。
func (m *Manager) SetContent(ctx context.Context, id, content string) error {
	content = strings.TrimSpace(content)
	format, err := Sniff([]byte(content))
	if err != nil {
		return err
	}
	return m.storeContent(ctx, id, []byte(content), format, "手动粘贴", nil)
}

// storeContent 落盘订阅内容并更新元数据；内容变了就重载内核。
// 先确认订阅存在（ID 合法且在列表里）再碰磁盘：ID 会拼进文件路径。
func (m *Manager) storeContent(ctx context.Context, id string, body []byte, format, via string, res *FetchResult) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	exists := m.findLocked(id) >= 0
	m.mu.Unlock()
	if !exists {
		return errNoSub
	}
	path := m.subFile(id)
	same := sameContent(path, body)
	if !same {
		if err := writeFileAtomic(path, body); err != nil {
			return err
		}
	}
	m.mu.Lock()
	i := m.findLocked(id)
	if i < 0 { // 写盘期间被删了（RemoveSub 也要拿 op，实际不会发生）
		m.mu.Unlock()
		_ = removeFile(path)
		return errNoSub
	}
	s := &m.st.Subs[i]
	now := time.Now().Unix()
	s.UpdatedAt, s.LastTryAt, s.LastError, s.Via, s.Format, s.Size = now, now, "", via, format, len(body)
	if res != nil {
		s.Info, s.Home = res.Info, res.Home
		s.HeaderIntervalMin = res.IntervalMin
		if res.FileName != "" && (s.Name == HostOf(s.URL) || s.Name == "") {
			s.Name = res.FileName
		}
	}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil || same {
		return err
	}
	if aerr := m.applyLocked(ctx); aerr != nil {
		return fmt.Errorf("订阅已保存，但内核没能加载：%v", aerr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 节点
// ---------------------------------------------------------------------------

// NodeView 节点列表的一行。
type NodeView struct {
	Node
	Selected bool `json:"selected"`
	Port     int  `json:"port,omitempty"`
	// Missing 勾选过、但已经不在订阅里（改名或下线）：它的代理口现在直接失败。
	Missing bool `json:"missing,omitempty"`
}

// Nodes 订阅里的节点（需要内核在运行：节点由内核解析）。
func (m *Manager) Nodes(ctx context.Context, id string) ([]NodeView, error) {
	m.mu.Lock()
	if m.findLocked(id) < 0 {
		m.mu.Unlock()
		return nil, errors.New("订阅不存在")
	}
	sel := map[string]int{}
	var order []string
	for _, s := range m.st.Selections {
		if s.SubID == id {
			sel[s.Node] = s.Port
			order = append(order, s.Node)
		}
	}
	m.mu.Unlock()
	if n, _ := statFile(m.subFile(id)); n <= 0 {
		return nil, errors.New("这个订阅还没有内容（更新失败的话可以粘贴订阅内容）")
	}
	api, err := m.api()
	if err != nil {
		return nil, errors.New("内核没在运行，启动内核后才能看到节点")
	}
	nodes, err := api.providerNodes(ctx, providerName(id))
	if err != nil {
		return nil, err
	}
	out := make([]NodeView, 0, len(nodes)+len(sel))
	seen := map[string]bool{}
	for _, n := range nodes {
		port, ok := sel[n.Name]
		seen[n.Name] = true
		out = append(out, NodeView{Node: n, Selected: ok, Port: port})
	}
	for _, name := range order {
		if !seen[name] {
			out = append(out, NodeView{Node: Node{Name: name}, Selected: true, Port: sel[name], Missing: true})
		}
	}
	return out, nil
}

// Select 设置这个订阅下勾选的节点（整体替换）。已勾选的节点保留原来的代理口，新勾选的分配新口。
func (m *Manager) Select(ctx context.Context, id string, nodes []string) error {
	want := []string{}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		want = append(want, n)
	}
	m.op.Lock()
	m.mu.Lock()
	if m.findLocked(id) < 0 {
		m.mu.Unlock()
		m.op.Unlock()
		return errors.New("订阅不存在")
	}
	oldPorts := map[string]int{}
	used := map[int]bool{}
	var others []Selection
	for _, s := range m.st.Selections {
		if s.SubID == id {
			oldPorts[s.Node] = s.Port
		} else {
			others = append(others, s)
			used[s.Port] = true
		}
	}
	if len(others)+len(want) > MaxSelections {
		m.mu.Unlock()
		m.op.Unlock()
		return fmt.Errorf("最多勾选 %d 个节点", MaxSelections)
	}
	for _, n := range want {
		if p, ok := oldPorts[n]; ok {
			used[p] = true
		}
	}
	next := others
	changed := len(oldPorts) != len(want)
	for _, n := range want {
		p, ok := oldPorts[n]
		if !ok {
			changed = true
			p = m.allocPortLocked(used)
			if p == 0 {
				m.mu.Unlock()
				m.op.Unlock()
				return errors.New("找不到空闲的本机端口")
			}
			used[p] = true
		}
		next = append(next, Selection{SubID: id, Node: n, Port: p})
	}
	if !changed {
		m.mu.Unlock()
		m.op.Unlock()
		return nil
	}
	m.st.Selections = next
	err := m.saveLocked()
	m.mu.Unlock()
	if err == nil {
		err = m.applyLocked(ctx)
	}
	m.op.Unlock()
	m.changed()
	return err
}

func (m *Manager) allocPortLocked(used map[int]bool) int {
	for p := m.opts.BasePort; p < m.opts.BasePort+2000 && p < 65536; p++ {
		if !used[p] && portFree(p) {
			return p
		}
	}
	return 0
}

// Delay 让内核测这些节点的延迟（并发 8 个，每个最多 6 秒）。不通的为 0。
func (m *Manager) Delay(ctx context.Context, id string, nodes []string) (map[string]int, error) {
	api, err := m.api()
	if err != nil {
		return nil, errors.New("内核没在运行")
	}
	if len(nodes) > 300 {
		nodes = nodes[:300]
	}
	out := make(map[string]int, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(n string) {
			defer func() { <-sem; wg.Done() }()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			d, err := api.nodeDelay(cctx, providerName(id), n, 6000)
			cancel()
			if err != nil {
				d = 0
			}
			mu.Lock()
			out[n] = d
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return out, nil
}

// ---------------------------------------------------------------------------
// 定时更新
// ---------------------------------------------------------------------------

func (m *Manager) runScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refreshDue(ctx)
		}
	}
}

// dueSubs 到了该更新的订阅：距上次成功超过间隔，且距上次尝试超过 30 分钟（失败后不连续重试）。
func (m *Manager) dueSubs(now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, s := range m.st.Subs {
		if s.URL == "" || m.refreshing[s.ID] {
			continue
		}
		if now.Unix()-s.UpdatedAt < int64(s.effectiveInterval())*60 {
			continue
		}
		if s.LastTryAt > s.UpdatedAt && now.Unix()-s.LastTryAt < int64(retryAfterFail/time.Second) {
			continue
		}
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) refreshDue(ctx context.Context) {
	for _, id := range m.dueSubs(time.Now()) {
		if ctx.Err() != nil {
			return
		}
		if err := m.RefreshSub(ctx, id); err != nil {
			m.logf("订阅 %s 定时更新失败：%v", id, err)
		}
	}
}
