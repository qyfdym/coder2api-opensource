package proxycore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultBasePort 节点代理口从这里往上分配。
	DefaultBasePort = 17890
	// MaxSubs / MaxSelections 订阅数与勾选节点数上限。
	MaxSubs       = 20
	MaxSelections = 100

	defaultIntervalMin = 24 * 60
	minIntervalMin     = 30
	// retryAfterFail 订阅更新失败后多久再试。
	retryAfterFail = 30 * time.Minute
	maxBackoff     = 30 * time.Second
)

// Sub 一个机场订阅。URL 等同密码，只在服务端使用，不下发给前端。
type Sub struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	UA          string `json:"ua,omitempty"`
	IntervalMin int    `json:"interval_min,omitempty"` // 0 = 按机场建议，没有建议则 24 小时
	// HeaderIntervalMin 机场在响应头里建议的更新间隔。
	HeaderIntervalMin int       `json:"header_interval_min,omitempty"`
	UpdatedAt         int64     `json:"updated_at,omitempty"`
	LastTryAt         int64     `json:"last_try_at,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	Via               string    `json:"via,omitempty"`
	Format            string    `json:"format,omitempty"`
	Size              int       `json:"size,omitempty"`
	Info              *UserInfo `json:"info,omitempty"`
	Home              string    `json:"home,omitempty"`
	AddedAt           int64     `json:"added_at,omitempty"`
}

func (s Sub) effectiveInterval() int {
	n := s.IntervalMin
	if n <= 0 {
		n = s.HeaderIntervalMin
	}
	if n <= 0 {
		n = defaultIntervalMin
	}
	if n < minIntervalMin {
		n = minIntervalMin
	}
	return n
}

type state struct {
	// Enabled 用户希望内核运行（面板里点了启动）。网关重启后据此自动拉起。
	Enabled    bool        `json:"enabled"`
	Mirror     string      `json:"mirror,omitempty"`
	ListenUser string      `json:"listen_user"`
	ListenPass string      `json:"listen_pass"`
	Subs       []Sub       `json:"subs"`
	Selections []Selection `json:"selections"`
}

// Options 装配参数。
type Options struct {
	// Dir 工作目录（state.json、bin/、run/ 都在这里，权限 0700）。
	Dir string
	// BasePort 节点代理口的起始端口；<=0 用 DefaultBasePort。
	BasePort int
	// BinPath 内核可执行文件；空 = Dir/bin/mihomo（Windows 加 .exe）。
	BinPath string
	// HTTP 拉订阅 / 下载内核用的客户端：proxy 为空 = 跟随环境变量，否则经该代理。nil = 内置实现。
	HTTP func(proxy string) *http.Client
	// FetchVia 直连拉订阅失败时依次尝试的代理（最多用前 2 个）。很多机场不让国内 IP 直接拉订阅，
	// 但允许经自己的节点拉。
	FetchVia func() []string
	// OnChange 节点代理口的集合变了（启停内核、勾选变化、删订阅）之后回调，用来同步代理池。
	OnChange func()
	Logf     func(format string, args ...any)
}

// Manager 订阅与内核的管理入口（进程内单例，并发安全）。
type Manager struct {
	opts      Options
	runDir    string
	binPath   string
	statePath string
	logs      *ring

	// op 串行化启停 / 重载 / 安装这类长操作。锁序：op → mu。
	op sync.Mutex

	mu         sync.Mutex
	st         state
	proc       *coreProc
	lastErr    string
	lastErrAt  int64
	restarts   int
	closed     bool
	install    InstallProgress
	refreshing map[string]bool
	binVer     string
	binMod     time.Time

	instDone, instTotal atomic.Int64
	// instMu 同一时间只装一个内核（下载和上传共用）。
	instMu sync.Mutex

	httpMu sync.Mutex
	https  map[string]*http.Client
}

// New 打开（必要时创建）工作目录并载入状态。
func New(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("proxycore: 工作目录为空")
	}
	// 一律用绝对路径：内核以 run/ 为工作目录启动，相对路径的可执行文件会找不到；
	// 热重载接口也要求配置文件是绝对路径。
	if abs, err := filepath.Abs(opts.Dir); err == nil {
		opts.Dir = abs
	}
	if opts.BinPath != "" {
		if abs, err := filepath.Abs(opts.BinPath); err == nil {
			opts.BinPath = abs
		}
	}
	if opts.BasePort <= 0 {
		opts.BasePort = DefaultBasePort
	}
	if opts.Logf == nil {
		opts.Logf = func(f string, a ...any) { log.Printf("[proxycore] "+f, a...) }
	}
	m := &Manager{
		opts:       opts,
		runDir:     filepath.Join(opts.Dir, "run"),
		statePath:  filepath.Join(opts.Dir, "state.json"),
		logs:       newRing(400),
		refreshing: map[string]bool{},
		https:      map[string]*http.Client{},
	}
	m.binPath = opts.BinPath
	if m.binPath == "" {
		name := "mihomo"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		m.binPath = filepath.Join(opts.Dir, "bin", name)
	}
	for _, d := range []string{opts.Dir, m.runDir, filepath.Join(m.runDir, "subs"), filepath.Dir(m.binPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("proxycore: 创建目录 %s: %w", d, err)
		}
	}
	if err := m.load(); err != nil {
		return m, err
	}
	return m, nil
}

func (m *Manager) load() error {
	b, err := os.ReadFile(m.statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("proxycore: 读取 %s: %w", m.statePath, err)
	default:
		if jerr := jsonUnmarshal(b, &m.st); jerr != nil {
			bak := fmt.Sprintf("%s.bad-%d", m.statePath, time.Now().Unix())
			_ = os.WriteFile(bak, b, 0o600)
			m.st = state{}
			err = fmt.Errorf("proxycore: %s 损坏（已备份为 %s，从空开始）: %w", m.statePath, bak, jerr)
		}
	}
	// ID 会拼进文件路径：状态文件里格式不对的条目直接丢弃。
	subs := m.st.Subs[:0:0]
	known := map[string]bool{}
	for _, s := range m.st.Subs {
		if ValidID(s.ID) && !known[s.ID] {
			known[s.ID] = true
			subs = append(subs, s)
		}
	}
	m.st.Subs = subs
	sels := m.st.Selections[:0:0]
	for _, s := range m.st.Selections {
		if known[s.SubID] && s.Port > 0 && s.Port < 65536 {
			sels = append(sels, s)
		}
	}
	m.st.Selections = sels
	if m.st.ListenUser == "" || m.st.ListenPass == "" {
		m.st.ListenUser, m.st.ListenPass = "c2a"+randHex(4), randHex(16)
		if serr := m.saveLocked(); serr != nil && err == nil {
			err = serr
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (m *Manager) saveLocked() error {
	b, err := jsonMarshalIndent(m.st)
	if err != nil {
		return err
	}
	return writeFileAtomic(m.statePath, b)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (m *Manager) subFile(id string) string { return filepath.Join(m.runDir, "subs", id+".txt") }

func (m *Manager) logf(format string, args ...any) { m.opts.Logf(format, args...) }

func (m *Manager) changed() {
	if m.opts.OnChange != nil {
		m.opts.OnChange()
	}
}

// ---------------------------------------------------------------------------
// 状态
// ---------------------------------------------------------------------------

// Status 内核状态（面板展示用）。
type Status struct {
	Installed   bool            `json:"installed"`
	Version     string          `json:"version,omitempty"`
	BinPath     string          `json:"bin_path"`
	Enabled     bool            `json:"enabled"`
	Running     bool            `json:"running"`
	PID         int             `json:"pid,omitempty"`
	StartedAt   int64           `json:"started_at,omitempty"`
	Restarts    int             `json:"restarts,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	LastErrorAt int64           `json:"last_error_at,omitempty"`
	Install     InstallProgress `json:"install"`
	Mirror      string          `json:"mirror,omitempty"`
	Listeners   int             `json:"listeners"`
	Platform    string          `json:"platform"`
	Asset       string          `json:"asset,omitempty"`
}

// installedVersion 内核版本（文件变了才重新运行 -v）。空 = 没安装或不可用。
func (m *Manager) installedVersion() string {
	fi, err := os.Stat(m.binPath)
	if err != nil || fi.IsDir() {
		return ""
	}
	m.mu.Lock()
	if m.binVer != "" && fi.ModTime().Equal(m.binMod) {
		v := m.binVer
		m.mu.Unlock()
		return v
	}
	m.mu.Unlock()
	v, verr := binaryVersion(context.Background(), m.binPath)
	if verr != nil {
		return ""
	}
	m.mu.Lock()
	m.binVer, m.binMod = v, fi.ModTime()
	m.mu.Unlock()
	return v
}

// Installed 内核文件存在且能运行。
func (m *Manager) Installed() bool { return m.installedVersion() != "" }

func (m *Manager) Status() Status {
	ver := m.installedVersion()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Installed: ver != "", Version: ver, BinPath: m.binPath, Enabled: m.st.Enabled,
		Restarts: m.restarts, LastError: m.lastErr, LastErrorAt: m.lastErrAt, Mirror: m.st.Mirror,
		Listeners: len(m.st.Selections), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	st.Install = m.install
	if st.Install.Running {
		st.Install.Done, st.Install.Total = m.instDone.Load(), m.instTotal.Load()
	}
	if a, err := AssetName(runtime.GOOS, runtime.GOARCH, "vX.Y.Z"); err == nil {
		st.Asset = a
	}
	if m.proc.alive() {
		st.Running, st.PID, st.StartedAt = true, m.proc.cmd.Process.Pid, m.proc.started.Unix()
	}
	return st
}

// Logs 最近 n 行内核日志。
func (m *Manager) Logs(n int) []string { return m.logs.tail(n) }

func (m *Manager) setErr(msg string) {
	m.mu.Lock()
	m.lastErr, m.lastErrAt = msg, time.Now().Unix()
	m.mu.Unlock()
	if msg != "" {
		m.logf("%s", msg)
	}
}

// ---------------------------------------------------------------------------
// 启停
// ---------------------------------------------------------------------------

// Boot = StartIfWanted + RunScheduler（阻塞到 ctx 结束）。
func (m *Manager) Boot(ctx context.Context) {
	m.StartIfWanted(ctx)
	m.RunScheduler(ctx)
}

// StartIfWanted 网关启动时调用：上次是运行状态就把内核拉起来（同步，约 1 秒）。要在 Kiro 预热、
// 余额刷新之前调用：节点代理口从一开始就在代理池里，内核没起来时请求会被拒绝，节点会被误判为连不上。
// 启动失败时在后台按退避重试。
func (m *Manager) StartIfWanted(ctx context.Context) {
	m.mu.Lock()
	want := m.st.Enabled
	m.mu.Unlock()
	if !want || !m.Installed() {
		return
	}
	m.op.Lock()
	if err := m.startLocked(ctx); err != nil { // 错误已由 startLocked 记录
		go m.retryStart(nil)
	}
	m.op.Unlock()
}

// RunScheduler 按计划更新订阅，阻塞到 ctx 结束。
func (m *Manager) RunScheduler(ctx context.Context) { m.runScheduler(ctx) }

// Start 启动内核（并记住「希望运行」，网关重启后自动拉起）。
func (m *Manager) Start(ctx context.Context) error {
	if !m.Installed() {
		return errors.New("内核还没安装")
	}
	m.op.Lock()
	m.mu.Lock()
	was := m.st.Enabled
	m.st.Enabled = true
	_ = m.saveLocked()
	m.mu.Unlock()
	err := m.ensureRunningLocked(ctx)
	m.op.Unlock()
	if !was {
		m.changed()
	}
	return err
}

// Stop 停止内核。勾选的节点随之退出代理池（不再分配给账号）。
func (m *Manager) Stop() error {
	m.op.Lock()
	m.mu.Lock()
	was := m.st.Enabled
	m.st.Enabled = false
	err := m.saveLocked()
	p := m.proc
	m.mu.Unlock()
	p.stop()
	m.op.Unlock()
	if was {
		m.changed()
	}
	return err
}

// Restart 重启内核。
func (m *Manager) Restart(ctx context.Context) error {
	if !m.Installed() {
		return errors.New("内核还没安装")
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	m.st.Enabled = true
	_ = m.saveLocked()
	p := m.proc
	m.mu.Unlock()
	p.stop()
	return m.startLocked(ctx)
}

// Shutdown 网关退出时结束内核（不改「希望运行」，下次启动自动拉起）。之后任何启动都会被拒绝。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true // 先标记：正在等 op 的请求拿到锁后也不会再起新内核
	m.mu.Unlock()
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	p := m.proc
	m.mu.Unlock()
	p.stop()
}

func (m *Manager) ensureRunningLocked(ctx context.Context) error {
	m.mu.Lock()
	p := m.proc
	m.mu.Unlock()
	if p.alive() {
		return m.reloadLocked(ctx, p)
	}
	return m.startLocked(ctx)
}

// writeConfigLocked 生成配置，交给内核 -t 检查，通过后才替换正式配置文件。返回正式配置的绝对路径。
func (m *Manager) writeConfigLocked(ctx context.Context, ctrl, secret string) (string, error) {
	if err := os.WriteFile(filepath.Join(m.runDir, guardFile), []byte(guardContent), 0o600); err != nil {
		return "", err
	}
	m.mu.Lock()
	in := buildInput{Controller: ctrl, Secret: secret, User: m.st.ListenUser, Pass: m.st.ListenPass,
		Selections: append([]Selection(nil), m.st.Selections...)}
	for _, s := range m.st.Subs {
		if fi, err := os.Stat(m.subFile(s.ID)); err == nil && fi.Size() > 0 {
			in.Subs = append(in.Subs, s.ID)
		}
	}
	m.mu.Unlock()
	data, err := buildConfig(in)
	if err != nil {
		return "", err
	}
	cfgNew := filepath.Join(m.runDir, "config.new.yaml")
	if err := os.WriteFile(cfgNew, data, 0o600); err != nil {
		return "", err
	}
	if err := validateConfig(ctx, m.binPath, m.runDir, cfgNew); err != nil {
		return "", err
	}
	cfg := filepath.Join(m.runDir, "config.yaml")
	if err := os.Rename(cfgNew, cfg); err != nil {
		return "", err
	}
	return filepath.Abs(cfg)
}

// lifecycleCtx 启停 / 重载用的 context：不跟着发起请求的页面一起取消（用户关了页面不能把内核停在半路），
// 但有自己的上限。
func lifecycleCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}

var errClosed = errors.New("网关正在退出")

func (m *Manager) startLocked(ctx context.Context) error {
	ctx, cancel := lifecycleCtx(ctx)
	defer cancel()
	if !m.Installed() {
		return errors.New("内核还没安装")
	}
	m.mu.Lock()
	old, closed := m.proc, m.closed
	m.mu.Unlock()
	if closed {
		return errClosed
	}
	old.stop()
	killStale(m.runDir)
	if moved := m.reclaimPortsLocked(); moved > 0 {
		m.logf("%d 个节点的本机端口被别的程序占用，已改用新端口", moved)
		defer m.changed()
	}
	port, err := freeLocalPort()
	if err != nil {
		return err
	}
	ctrl, secret := "127.0.0.1:"+strconv.Itoa(port), randHex(16)
	cfg, err := m.writeConfigLocked(ctx, ctrl, secret)
	if err != nil {
		m.setErr(err.Error())
		return err
	}
	p, err := startProc(ctx, m.binPath, m.runDir, cfg, ctrl, secret, m.logs)
	if err != nil {
		m.setErr(err.Error())
		return err
	}
	m.mu.Lock()
	m.proc, m.lastErr, m.lastErrAt = p, "", 0
	n := len(m.st.Selections)
	m.mu.Unlock()
	m.logf("内核已启动（PID %d，%d 个节点代理口）", p.cmd.Process.Pid, n)
	go m.supervise(p)
	return nil
}

// reloadLocked 让运行中的内核换上新配置；重载失败就重启，重启也失败就交给后台按退避重试
// （这时旧内核已经停了，不能让它就这么停着）。
func (m *Manager) reloadLocked(ctx context.Context, p *coreProc) error {
	ctx, cancel := lifecycleCtx(ctx)
	defer cancel()
	cfg, err := m.writeConfigLocked(ctx, p.ctrl, p.secret)
	if err != nil {
		return err
	}
	api := &apiClient{base: "http://" + p.ctrl, secret: p.secret, hc: &http.Client{Timeout: 30 * time.Second}}
	if err := api.reload(ctx, cfg); err != nil {
		m.logf("热重载失败，重启内核：%v", err)
		p.stop()
		if err := m.startLocked(ctx); err != nil {
			go m.retryStart(p)
			return err
		}
	}
	return nil
}

// reclaimPortsLocked 启动前检查勾选节点的端口：被别的程序占了就换一个（否则那个程序会收到发往节点的
// 流量和代理口的账号密码）。返回换了端口的个数。调用方持有 op，内核此时没在运行。
func (m *Manager) reclaimPortsLocked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	used := map[int]bool{}
	for _, s := range m.st.Selections {
		used[s.Port] = true
	}
	moved := 0
	for i, s := range m.st.Selections {
		if portFree(s.Port) {
			continue
		}
		if p := m.allocPortLocked(used); p != 0 {
			used[p] = true
			m.st.Selections[i].Port = p
			moved++
		}
	}
	if moved > 0 {
		_ = m.saveLocked()
	}
	return moved
}

// applyLocked 订阅内容或勾选变了之后调用：内核在跑就热重载，希望运行但没在跑就启动。
func (m *Manager) applyLocked(ctx context.Context) error {
	m.mu.Lock()
	want := m.st.Enabled
	m.mu.Unlock()
	if !want || !m.Installed() {
		return nil
	}
	return m.ensureRunningLocked(ctx)
}

// supervise 内核意外退出时自动重启（指数退避，最长 30 秒）。
func (m *Manager) supervise(p *coreProc) {
	<-p.done
	if p.stopping.Load() {
		return
	}
	m.mu.Lock()
	m.restarts++
	m.mu.Unlock()
	m.setErr(fmt.Sprintf("内核意外退出（%v）：%s", p.exitErr, strings.Join(m.logs.tail(2), " | ")))
	m.retryStart(p)
}

func (m *Manager) retryStart(p *coreProc) {
	delay := time.Second
	for {
		time.Sleep(delay)
		m.op.Lock()
		m.mu.Lock()
		ok := !m.closed && m.st.Enabled && m.proc == p
		m.mu.Unlock()
		if !ok {
			m.op.Unlock()
			return
		}
		err := m.startLocked(context.Background())
		m.op.Unlock()
		if err == nil {
			return
		}
		if delay *= 2; delay > maxBackoff {
			delay = maxBackoff
		}
	}
}

func (m *Manager) api() (*apiClient, error) {
	m.mu.Lock()
	p := m.proc
	m.mu.Unlock()
	if !p.alive() {
		return nil, errNotRunning
	}
	return &apiClient{base: "http://" + p.ctrl, secret: p.secret, hc: &http.Client{Timeout: 20 * time.Second}}, nil
}

// ---------------------------------------------------------------------------
// 节点代理口（代理池成员）
// ---------------------------------------------------------------------------

func (m *Manager) nodeURLLocked(s Selection) string {
	return "http://" + listenUser(m.st.ListenUser, s) + ":" + m.st.ListenPass + "@127.0.0.1:" + strconv.Itoa(s.Port)
}

// NodeURLs 勾选节点的本机代理口（带认证的完整地址）。内核没被启动时为空：停掉内核 = 这些节点退出代理池。
// 内核被启动了但意外退出时照样返回：请求会失败并顺延到池里的其它代理，不会悄悄改走直连。
func (m *Manager) NodeURLs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.st.Enabled {
		return nil
	}
	out := make([]string, 0, len(m.st.Selections))
	for _, s := range m.st.Selections {
		out = append(out, m.nodeURLLocked(s))
	}
	return out
}

// NodeLabel 池成员地址如果是某个订阅节点的代理口，返回订阅名与节点名。
func (m *Manager) NodeLabel(raw string) (sub, node string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" {
		return "", "", false
	}
	port, _ := strconv.Atoi(u.Port())
	m.mu.Lock()
	defer m.mu.Unlock()
	if u.User == nil {
		return "", "", false
	}
	for _, s := range m.st.Selections {
		if s.Port == port && u.User.Username() == listenUser(m.st.ListenUser, s) {
			name := s.SubID
			for _, x := range m.st.Subs {
				if x.ID == s.SubID {
					name = x.Name
				}
			}
			return name, s.Node, true
		}
	}
	return "", "", false
}

// IsNodeURL 是否是订阅节点的代理口。
func (m *Manager) IsNodeURL(raw string) bool {
	_, _, ok := m.NodeLabel(raw)
	return ok
}

// ---------------------------------------------------------------------------
// HTTP 客户端
// ---------------------------------------------------------------------------

func (m *Manager) httpClient(proxy string) *http.Client {
	if m.opts.HTTP != nil {
		return m.opts.HTTP(proxy)
	}
	m.httpMu.Lock()
	defer m.httpMu.Unlock()
	if c, ok := m.https[proxy]; ok {
		return c
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			t.Proxy = http.ProxyURL(u)
		}
	}
	c := &http.Client{Transport: t}
	m.https[proxy] = c
	return c
}

// maskProxy 去掉代理地址里的账号密码。
func maskProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "（无效地址）"
	}
	return u.Scheme + "://" + u.Host
}

// ---------------------------------------------------------------------------
// 安装
// ---------------------------------------------------------------------------

// StartInstall 后台下载并安装最新版内核（进度见 Status().Install）。下载完先对照 GitHub 接口
// （直连，不经镜像）给出的 SHA-256：不一致就拒绝；拿不到校验值（接口不通 / 限流）时照样安装，
// 但标成「未校验」并给出文件的 SHA-256，供手动核对。
func (m *Manager) StartInstall(mirror string) error {
	mirror, err := NormalizeMirror(mirror)
	if err != nil {
		return err
	}
	if !m.instMu.TryLock() {
		return errors.New("正在安装内核，请等它完成")
	}
	m.mu.Lock()
	m.install = InstallProgress{Running: true, Stage: "准备", At: time.Now().Unix()}
	m.instDone.Store(0)
	m.instTotal.Store(0)
	m.st.Mirror = mirror
	_ = m.saveLocked()
	m.mu.Unlock()
	go func() {
		defer m.instMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		stage := func(s string) {
			m.mu.Lock()
			m.install.Stage = s
			m.mu.Unlock()
		}
		res := InstallProgress{At: time.Now().Unix(), Stage: "完成"}
		data, ver, asset, err := download(ctx, m.httpClient(""), mirror, stage, &m.instDone, &m.instTotal)
		if err == nil {
			stage("校验")
			res.SHA256 = sha256Hex(data)
			want, derr := releaseDigest(ctx, m.httpClient(""), ver, asset)
			switch {
			case derr == nil && !strings.EqualFold(want, res.SHA256):
				err = fmt.Errorf("下载的文件和 GitHub 公布的 SHA-256 不一致（%s），已拒绝安装：镜像可能被篡改", truncateStr(res.SHA256, 16))
			case derr == nil:
				res.Verified = true
			default:
				m.logf("拿不到 %s 的官方 SHA-256（%v），安装但标记为未校验", asset, derr)
			}
		}
		if err == nil {
			stage("安装")
			res.Version, err = m.installData(ctx, data)
		}
		if err != nil {
			res.Error, res.Stage = err.Error(), "失败"
			m.logf("安装内核失败：%v", err)
		} else {
			m.logf("内核已安装：%s（SHA-256 %s，%s）", res.Version, res.SHA256, map[bool]string{true: "已对照 GitHub 校验", false: "未校验"}[res.Verified])
		}
		m.mu.Lock()
		m.install = res
		m.mu.Unlock()
	}()
	return nil
}

// InstallFrom 安装上传的内核文件（.gz / .zip / 可执行文件）。
func (m *Manager) InstallFrom(ctx context.Context, r io.Reader) (string, error) {
	if !m.instMu.TryLock() {
		return "", errors.New("正在安装内核，请等它完成")
	}
	defer m.instMu.Unlock()
	data, err := io.ReadAll(io.LimitReader(r, maxDownload+1))
	if err != nil {
		return "", fmt.Errorf("读取上传内容失败：%v", err)
	}
	if len(data) > maxDownload {
		return "", fmt.Errorf("文件超过 %d MB", maxDownload>>20)
	}
	ver, err := m.installData(ctx, data)
	if err == nil {
		m.mu.Lock()
		m.install = InstallProgress{At: time.Now().Unix(), Stage: "完成", Version: ver, SHA256: sha256Hex(data)}
		m.mu.Unlock()
		m.logf("已安装上传的内核：%s（SHA-256 %s）", ver, sha256Hex(data))
	}
	return ver, err
}

// installData 解压、确认是 mihomo，然后替换内核文件；内核原本在运行就用新内核重新启动。
// 调用方持有 instMu。
func (m *Manager) installData(ctx context.Context, data []byte) (string, error) {
	suffix := ".tmp"
	if runtime.GOOS == "windows" {
		suffix = ".tmp.exe"
	}
	f, err := os.CreateTemp(filepath.Dir(m.binPath), "mihomo-*"+suffix)
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_ = f.Close()
	if err := extractBinary(data, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	ver, err := binaryVersion(ctx, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	p, want, closed := m.proc, m.st.Enabled, m.closed
	m.mu.Unlock()
	if closed {
		_ = os.Remove(tmp)
		return "", errClosed
	}
	p.stop() // 用新内核重启；Windows 上运行中的可执行文件也替换不了
	if runtime.GOOS == "windows" {
		_ = os.Remove(m.binPath)
	}
	if err := os.Rename(tmp, m.binPath); err != nil {
		_ = os.Remove(tmp)
		if want {
			go m.retryStart(p)
		}
		return "", fmt.Errorf("替换内核文件失败：%v", err)
	}
	_ = os.Chmod(m.binPath, 0o755)
	m.mu.Lock()
	m.binVer, m.binMod = "", time.Time{}
	m.mu.Unlock()
	if want {
		if err := m.startLocked(ctx); err != nil {
			go m.retryStart(p)
			return ver, fmt.Errorf("内核 %s 已安装，但启动失败（后台会继续重试）：%v", ver, err)
		}
	}
	return ver, nil
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

func sameContent(path string, data []byte) bool {
	old, err := os.ReadFile(path)
	return err == nil && bytes.Equal(old, data)
}

func statFile(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
