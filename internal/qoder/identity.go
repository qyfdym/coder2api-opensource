package qoder

// 机器身份：官方 UMID 组件（runtime-info）给出的真实设备身份。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：desktop_install_dir / _read_ini / runtime_info_exe / run_runtime_info /
// native_machine_identity / _first_identity_with_vote / _identity_vote_key / _sample_identity /
// _load_identity_cache / _save_identity_cache / _purge_identity_seed / _consume_identity_reset。
//
// 为什么非要真身份：官方桌面端调用活动平台前，会 spawn 自己的原生组件取机器身份
//
//	<install>/resources/umid/runtime-info.exe prod --account-stdin
//	stdin: {"account": "<uid>"}    stdout: {"machineToken","machineType","machineCode","vmInfo",...}
//
// 服务端**按这些值过滤设备定向活动**：派生的假身份不会报错，但列表里会静默少掉「每日领取
// 100 Credits」（原实现 issue #10 实测：全套派生头 → 整条过滤）。所以本包只用组件给出的真值，
// 取不到就**不发**机器头并在结论里说明原因，绝不发派生假头。
//
// 身份是**机器级**的：同一台机器上不同账号、甚至空账号 id 都返回同一份（原实现实测）。所以这里
// 只缓存一份，与区域、账号无关；也因此同一台机器上的多个账号无法「各自」领到按设备去重的活动。
//
// 组件输出由**种子文件** ~/.config/.locale_cfg 决定：同种子下重复调用返回同一身份，删种子才换新。
// 缓存纪律（原实现 #18 设计）：
//   - 缓存优先：落盘命中就不调组件（容器重建 / 升级后身份不变的唯一来源）；
//   - 首次落盘前做自适应多数表决（组件的 VM 判定会在 KVM/13 ↔ Docker/50 之间抖动，实测
//     1.56%~10.29%，单次采样会把用户永久固定到少数派）；
//   - 只有显式换身份的路径才清种子：服务端不认可时的自愈（force）、ResetMachineIdentity、
//     落盘 TTL 到期轮换；其余路径（缓存命中、组件失败回退、首次获取）一律不动种子；
//   - 组件不可用时回退落盘缓存（哪怕已过期）——比没有身份好。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ErrRuntimeInfoNotFound 本机找不到 runtime-info 组件（用 errors.Is 判断）。
// Windows：安装官方桌面客户端即可；Linux / macOS：显式调用 FetchRuntimeInfo 提取到 DataDir/umid/。
var ErrRuntimeInfoNotFound = errors.New("未找到官方 UMID 组件 runtime-info")

// MachineIdentity 官方组件给出的真实机器身份（对外只读快照）。
type MachineIdentity struct {
	MachineToken string
	MachineType  string
	MachineCode  string
	IsVM         bool // 组件 vmInfo.isVm（纯机器指纹：开启 VBS/HVCI 的实体机也可能被判为虚拟机）
	// Real 恒为 true：本包只返回组件给出的身份（含其落盘缓存），从不返回派生身份。
	Real   bool
	Source string // IdentitySourceNative（"runtime-info"）

	VMBrand    string         // vmInfo.brand（如 KVM / Docker / Hyper-V）
	VMTypeCode int64          // vmInfo.vmTypeCode
	VMScore    float64        // vmInfo.percentage（风控评分 0-100）
	VMInfo     map[string]any // 组件原样输出的 vmInfo

	Path      string    // 产出该身份的组件路径（落盘命中时为空）
	FromCache bool      // 来自落盘缓存（本次未调用组件）
	Stale     bool      // 组件当前不可用 / 执行失败，回退到了落盘缓存（可能已过 TTL）
	CachedAt  time.Time // 落盘时间（未落盘为零值）
}

// MachineIdentity 取本机真实机器身份（内存 30 分钟 → 落盘缓存 → 调组件）。
//
// 组件解析顺序：Options.RuntimeInfoPath → Windows 官方桌面客户端安装目录
// （%LOCALAPPDATA%\<Qoder CN|QoderCN|Qoder>\<… Launcher>\state.ini 的 installDir、
// 或 %LOCALAPPDATA%\Programs\<…>）下的 resources\umid\runtime-info.exe → DataDir/umid/runtime-info。
// 找不到组件且没有落盘缓存时返回 (nil, 包装了 ErrRuntimeInfoNotFound 的错误)；
// 组件存在但执行失败时返回 (nil, 原因)，并在 30 分钟内不再反复拉起组件。
func (c *Client) MachineIdentity(ctx context.Context) (*MachineIdentity, error) {
	return c.machineIdentity(ctx, false)
}

// RuntimeInfoLocation 当前能找到的组件路径与已查找的候选（诊断 / 面板展示用；不执行组件）。
func (c *Client) RuntimeInfoLocation() (path string, searched []string) {
	return c.findRuntimeInfo()
}

// ResetMachineIdentity 主动换身份（原实现 QD_MACHINE_IDENTITY_RESET=1）：清内存缓存、
// 删除落盘缓存，并删除组件种子——只清缓存会复刻出一个完全相同的身份（身份由种子决定）。
// 下一次 MachineIdentity 会重新表决并落盘。
func (c *Client) ResetMachineIdentity() error {
	st := c.identity
	st.mu.Lock()
	defer st.mu.Unlock()
	st.mem, st.failErr = nil, nil
	var err error
	if p := c.identityCachePath(); p != "" {
		switch e := os.Remove(p); {
		case e == nil:
			c.logf("[machine-identity] INFO: machine identity cache cleared (reset)")
		case !os.IsNotExist(e):
			err = e
		}
	}
	c.purgeIdentitySeed()
	return err
}

// machineIdentity 解析主流程。force=true 是「服务端不认可当前身份」的自愈路径：跳过全部缓存、
// 清种子换新身份、单次采样并**覆盖**落盘（否则重建后仍会用被拒的旧身份）。
func (c *Client) machineIdentity(ctx context.Context, force bool) (*MachineIdentity, error) {
	st := c.identity
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.clock()
	cacheOn := c.identityCachePath() != ""
	ttlRotation := false
	if !force {
		if st.mem != nil && now.Sub(st.memAt) < st.memTTL {
			return st.view(), nil
		}
		if st.failErr != nil && now.Sub(st.failAt) < st.memTTL {
			return nil, st.failErr
		}
		if cacheOn {
			if rec := c.loadIdentityCache(now, false); rec != nil {
				st.setMem(rec, now, "", false, true)
				if st.once("reuse") {
					c.logf("[machine-identity] INFO: machine identity cache: reusing cached identity (rebuilds keep the same device)")
				}
				return st.view(), nil
			}
			// TTL 到期（有记录但已过期）＝「定期轮换」：与「首次无缓存」区分开，只有它才清种子。
			ttlRotation = c.identityCacheExpired(now)
		}
	}
	exe, searched := c.findRuntimeInfo()
	if exe == "" {
		if cacheOn {
			if rec := c.loadIdentityCache(now, true); rec != nil {
				st.setMem(rec, now, "", true, true)
				return st.view(), nil
			}
		}
		st.mem = nil
		err := fmt.Errorf("%w（已查找：%s）", ErrRuntimeInfoNotFound, strings.Join(searched, "；"))
		if st.once("not-found") {
			c.logf("[runtime-info] %v；签到 / 活动请求将不发送机器头", err)
		}
		return nil, err
	}
	if force || ttlRotation {
		c.purgeIdentitySeed()
	}
	var rec *identityRecord
	var runErr error
	if cacheOn && !force && !ttlRotation && st.vote {
		rec, runErr = c.voteIdentity(ctx, exe)
	} else {
		rec, runErr = c.sampleIdentity(ctx, exe)
	}
	if rec != nil {
		if cacheOn {
			if kept := c.saveIdentityCache(rec, force, now); kept != nil {
				rec = kept
			}
		}
		st.failErr = nil
		st.setMem(rec, now, exe, false, false)
		return st.view(), nil
	}
	if cacheOn {
		if stored := c.loadIdentityCache(now, true); stored != nil {
			st.setMem(stored, now, exe, true, true)
			return st.view(), nil
		}
	}
	st.mem = nil
	err := fmt.Errorf("runtime-info 调用失败（%s）：%w", exe, runErr)
	if ctx.Err() == nil {
		// 只有组件自己失败才做负缓存；调用方 ctx 到期不代表组件坏了。
		st.failErr, st.failAt = err, now
	}
	return nil, err
}

func (s *identityState) setMem(rec *identityRecord, now time.Time, path string, stale, disk bool) {
	s.mem, s.memAt, s.memPath, s.memStale, s.memDisk = rec, now, path, stale, disk
}

// view 内存记录 → 对外快照（拷贝，调用方修改不影响缓存）。
func (s *identityState) view() *MachineIdentity {
	r := s.mem
	vm := make(map[string]any, len(r.VMInfo))
	for k, v := range r.VMInfo {
		vm[k] = v
	}
	out := &MachineIdentity{
		MachineToken: r.MachineToken,
		MachineType:  r.MachineType,
		MachineCode:  r.MachineCode,
		IsVM:         r.VM,
		Real:         true,
		Source:       oaFirst(r.Source, IdentitySourceNative),
		VMBrand:      oaStr(vm["brand"]),
		VMTypeCode:   oaInt(vm["vmTypeCode"]),
		VMScore:      oaNum(vm["percentage"]),
		VMInfo:       vm,
		Path:         s.memPath,
		FromCache:    s.memDisk,
		Stale:        s.memStale,
	}
	if r.CachedAt > 0 {
		out.CachedAt = time.Unix(int64(r.CachedAt), 0)
	}
	return out
}

// ---------------------------------------------------------------------------
// 组件定位
// ---------------------------------------------------------------------------

func (c *Client) findRuntimeInfo() (string, []string) {
	var searched []string
	check := func(p string) bool {
		searched = append(searched, p)
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular()
	}
	if p := strings.TrimSpace(c.opts.RuntimeInfoPath); p != "" && check(p) {
		return p, searched
	}
	if runtime.GOOS == "windows" {
		dirs := desktopInstallDirs
		if c.identity.installDirs != nil {
			dirs = c.identity.installDirs
		}
		for _, d := range dirs() {
			if p := filepath.Join(d, "resources", "umid", "runtime-info.exe"); check(p) {
				return p, searched
			}
		}
	}
	if dd := strings.TrimSpace(c.opts.DataDir); dd != "" {
		names := []string{"runtime-info"}
		if runtime.GOOS == "windows" {
			names = []string{"runtime-info.exe", "runtime-info"}
		}
		for _, n := range names {
			if p := filepath.Join(dd, "umid", n); check(p) {
				return p, searched
			}
		}
	}
	return "", searched
}

// desktopInstallDirs Windows 官方桌面客户端安装目录候选（launcher state.ini 的 installDir 优先，
// 其次 %LOCALAPPDATA%\Programs\<name>）。国内版与国际版的目录名都纳入（身份是机器级的，哪个在都行）。
func desktopInstallDirs() []string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Local")
	}
	names := []string{"Qoder CN", "QoderCN", "Qoder"}
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[strings.ToLower(d)] {
			return
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			seen[strings.ToLower(d)] = true
			out = append(out, d)
		}
	}
	for _, name := range names {
		for _, launcher := range []string{name + " Launcher", "Launcher"} {
			add(readIniValue(filepath.Join(base, name, launcher, "state.ini"), "installDir"))
		}
	}
	for _, name := range names {
		add(filepath.Join(base, "Programs", name))
	}
	return out
}

// readIniValue 读 launcher 的 state.ini（UTF-8 或 UTF-16），返回 key=value 的 value；找不到为空串。
// 先按 UTF-8 找、找不到再按 UTF-16 找（无 BOM 的 UTF-16LE 也是合法 UTF-8 字节流，只是找不到键）。
func readIniValue(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	find := func(text string) string {
		prefix := strings.ToLower(key) + "="
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToLower(line), prefix) {
				return strings.TrimSpace(line[len(prefix):])
			}
		}
		return ""
	}
	if v := find(strings.TrimPrefix(string(raw), "\ufeff")); v != "" {
		return v
	}
	return find(iniDecodeUTF16(raw))
}

// iniDecodeUTF16 按 BOM 判断字节序（无 BOM 按小端，与 Windows / Python "utf-16" 缺省一致）。
func iniDecodeUTF16(b []byte) string {
	big := false
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		big, b = true, b[2:]
	} else if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if big {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// ---------------------------------------------------------------------------
// 组件调用
// ---------------------------------------------------------------------------

// runtimeInfoError 组件调用失败（category 用于「同类失败进程内只提示一次」）。
type runtimeInfoError struct {
	category string
	msg      string
	err      error
}

func (e *runtimeInfoError) Error() string { return e.msg }
func (e *runtimeInfoError) Unwrap() error { return e.err }

// runtimeInfoStdin 组件 stdin（与原实现 json.dumps({"account": id}) + b" " 逐字节一致）。
func runtimeInfoStdin(account string) []byte {
	s, _ := json.Marshal(account)
	return []byte(`{"account": ` + string(s) + `} `)
}

// defaultRuntimeInfoRunner 生产实现：exe prod --account-stdin，工作目录为组件所在目录。
// 与原实现一致：只要 stdout 有内容就交给上层解析，不看退出码。
func defaultRuntimeInfoRunner(ctx context.Context, exe string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, "prod", "--account-stdin")
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if out := bytes.TrimSpace(stdout.Bytes()); len(out) > 0 {
		return out, nil
	}
	if ctx.Err() != nil {
		return nil, &runtimeInfoError{category: "timeout", msg: fmt.Sprintf("%s 调用超时或被取消：%v", exe, ctx.Err()), err: ctx.Err()}
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		// 文件存在但 exec 失败：缺的通常不是组件本身，而是它的动态 loader（glibc 的
		// /lib64/ld-linux-x86-64.so.2，alpine/musl 没有）——原样透出，避免被误读成「路径没配好」。
		return nil, &runtimeInfoError{category: "exec", err: err, msg: fmt.Sprintf(
			"无法执行 %s：%v（组件存在但跑不起来；Docker/alpine 需要 gcompat libstdc++ libgcc 兼容层）", exe, err)}
	}
	code := 0
	if ee != nil {
		code = ee.ExitCode()
	}
	return nil, &runtimeInfoError{category: "empty-output", err: err, msg: fmt.Sprintf(
		"%s 运行结束但没有输出（exit=%d）%s", exe, code, oaCut(strings.TrimSpace(stderr.String()), 200))}
}

// sampleIdentity 单次调用组件并构造记录；无输出 / 非 JSON / 字段不全都返回错误。
func (c *Client) sampleIdentity(ctx context.Context, exe string) (*identityRecord, error) {
	st := c.identity
	run := st.run
	if run == nil {
		run = defaultRuntimeInfoRunner
	}
	rctx, cancel := context.WithTimeout(ctx, st.runTimeout)
	defer cancel()
	// 身份是机器级的：账号字段传空串即可（原实现在无账号上下文时同样如此）。
	out, err := run(rctx, exe, runtimeInfoStdin(""))
	if err != nil {
		cat := "run"
		var rie *runtimeInfoError
		if errors.As(err, &rie) {
			cat = rie.category
		}
		if st.once("warn:" + cat) {
			c.logf("[runtime-info] %v", err)
		}
		return nil, err
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(line), &data); err != nil {
		e := &runtimeInfoError{category: "json", err: err, msg: fmt.Sprintf("%s 输出不是 JSON：%v", exe, err)}
		if st.once("warn:json") {
			c.logf("[runtime-info] %v", e)
		}
		return nil, e
	}
	rec := &identityRecord{
		MachineToken: strings.TrimSpace(oaStr(data["machineToken"])),
		MachineType:  strings.TrimSpace(oaStr(data["machineType"])),
		MachineCode:  strings.TrimSpace(oaStr(data["machineCode"])),
		VMInfo:       oaMap(data["vmInfo"]),
		Source:       IdentitySourceNative,
	}
	if rec.VMInfo == nil {
		rec.VMInfo = map[string]any{}
	}
	rec.VM = oaTruthy(rec.VMInfo["isVm"])
	if rec.MachineToken == "" || rec.MachineType == "" || rec.MachineCode == "" {
		return nil, &runtimeInfoError{category: "fields", msg: exe + " 输出缺少 machineToken / machineType / machineCode"}
	}
	return rec, nil
}

// identityVoteKey 表决键：token + type / code / isVm / brand / vmTypeCode。
// 刻意排除 vmInfo.percentage 这类连续噪声字段——它们的波动不应破坏多数表决；
// 选中后落盘的是该样本的**完整** vmInfo（天然自洽，不逐字段杂交）。
func identityVoteKey(r *identityRecord) string {
	vm := r.VMInfo
	return strings.Join([]string{r.MachineToken, r.MachineType, r.MachineCode,
		strconv.FormatBool(oaTruthy(vm["isVm"])), oaStr(vm["brand"]),
		strconv.FormatInt(voteTypeCode(vm["vmTypeCode"]), 10)}, "\x00")
}

// voteTypeCode 原实现 int(vm.get("vmTypeCode") or 0)，非法为 -1。
func voteTypeCode(v any) int64 {
	if !oaTruthy(v) {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case bool:
		return 1
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// identityBranch 样本的（品牌，vmTypeCode 原文），用于表决日志。
func identityBranch(r *identityRecord) (string, string) {
	brand := oaFirst(oaStr(r.VMInfo["brand"]), "unknown")
	v, ok := r.VMInfo["vmTypeCode"]
	if !ok || v == nil {
		return brand, "None"
	}
	return brand, fmt.Sprint(v)
}

// voteIdentity 首次落盘前的**自适应**多数表决：先采样 voteRounds 次；全一致直接采纳；出现分歧
// 再补 voteExtend 次（上限有界，首次延迟可控），取出现次数最多的**完整样本**。
// 唯一多数 → 采纳；平票 / 全分歧 → 取首个样本（确定性）。全部采样失败返回最后一个错误。
func (c *Client) voteIdentity(ctx context.Context, exe string) (*identityRecord, error) {
	st := c.identity
	var lastErr error
	take := func(n int) []*identityRecord {
		var got []*identityRecord
		for i := 0; i < n && ctx.Err() == nil; i++ {
			rec, err := c.sampleIdentity(ctx, exe)
			if rec != nil {
				got = append(got, rec)
			} else {
				lastErr = err
			}
		}
		return got
	}
	rounds := st.voteRounds
	if rounds < 1 {
		rounds = 1
	}
	samples := take(rounds)
	if len(samples) == 0 {
		if lastErr == nil {
			lastErr = ctx.Err()
		}
		return nil, lastErr
	}
	type bucket struct {
		key   string
		items []*identityRecord
	}
	tally := func() []*bucket {
		var bs []*bucket
		idx := map[string]*bucket{}
		for _, s := range samples {
			k := identityVoteKey(s)
			b := idx[k]
			if b == nil {
				b = &bucket{key: k}
				idx[k] = b
				bs = append(bs, b)
			}
			b.items = append(b.items, s)
		}
		return bs
	}
	bs := tally()
	if len(bs) == 1 {
		brand, vtc := identityBranch(samples[0])
		c.logf("[machine-identity] INFO: identity vote: %d/%d %s (vmTypeCode=%s)", len(samples), len(samples), brand, vtc)
		return samples[0], nil
	}
	samples = append(samples, take(st.voteExtend)...)
	bs = tally()
	top := bs[0]
	for _, b := range bs[1:] {
		if len(b.items) > len(top.items) {
			top = b
		}
	}
	ties := 0
	for _, b := range bs {
		if len(b.items) == len(top.items) {
			ties++
		}
	}
	total := len(samples)
	var picked *identityRecord
	var summary string
	if ties == 1 {
		picked = top.items[0]
		brand, vtc := identityBranch(picked)
		var minority []string
		for _, b := range bs {
			if b != top {
				name, _ := identityBranch(b.items[0])
				minority = append(minority, name)
			}
		}
		summary = fmt.Sprintf("%d rounds disagreed; extended to %d -> %d/%d %s (%d minority %s; vmTypeCode=%s)",
			rounds, total, len(top.items), total, brand, total-len(top.items), strings.Join(minority, "/"), vtc)
	} else {
		picked = samples[0]
		var dist []string
		for _, b := range bs {
			name, _ := identityBranch(b.items[0])
			dist = append(dist, fmt.Sprintf("%s %d", name, len(b.items)))
		}
		brand, vtc := identityBranch(picked)
		summary = fmt.Sprintf("%d rounds disagreed; extended to %d -> no majority (%s); using first sample (%s, vmTypeCode=%s)",
			rounds, total, strings.Join(dist, " / "), brand, vtc)
	}
	c.logf("[machine-identity] INFO: identity vote: %s", summary)
	return picked, nil
}

// ---------------------------------------------------------------------------
// 落盘缓存（DataDir/machine_identity.json；格式与原实现一致：{"version":1,"realm":{"cn":{…},"intl":{…}}}）
// ---------------------------------------------------------------------------

// identityRealmKeys 身份是机器级的，两个区域写同一份；读取时国内版优先。
var identityRealmKeys = []string{RealmCN, RealmIntl}

func (c *Client) identityCachePath() string {
	dd := strings.TrimSpace(c.opts.DataDir)
	if dd == "" {
		return ""
	}
	return filepath.Join(dd, "machine_identity.json")
}

// readIdentityDoc 读缓存文档；缺失返回空壳；损坏 / 结构异常返回空壳（并首次 WARN）。绝不失败。
func (c *Client) readIdentityDoc() map[string]any {
	empty := map[string]any{"version": 1, "realm": map[string]any{}}
	raw, err := os.ReadFile(c.identityCachePath())
	if err != nil {
		if !os.IsNotExist(err) && c.identity.once("cache-corrupt") {
			c.logf("[machine-identity] WARN: machine identity cache unreadable (%v); regenerating", err)
		}
		return empty
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || oaMap(doc["realm"]) == nil {
		if c.identity.once("cache-corrupt") {
			c.logf("[machine-identity] WARN: machine identity cache unreadable (unexpected structure); regenerating")
		}
		return empty
	}
	return doc
}

// entryToRecord 缓存条目 → 记录；字段不全 / 类型不对返回 nil（视为无缓存）。
func entryToRecord(v any) *identityRecord {
	m := oaMap(v)
	if m == nil {
		return nil
	}
	rec := &identityRecord{
		MachineToken: strings.TrimSpace(oaStr(m["machineToken"])),
		MachineType:  strings.TrimSpace(oaStr(m["machineType"])),
		MachineCode:  strings.TrimSpace(oaStr(m["machineCode"])),
		VM:           oaTruthy(m["vm"]),
		VMInfo:       oaMap(m["vm_info"]),
		Source:       oaFirst(oaStr(m["source"]), IdentitySourceNative),
		CachedAt:     oaNum(m["cached_at"]),
	}
	if rec.VMInfo == nil {
		rec.VMInfo = map[string]any{}
	}
	if rec.MachineToken == "" || rec.MachineType == "" || rec.MachineCode == "" {
		return nil
	}
	return rec
}

// cachedEntry 取第一条有效条目（国内版优先）；present 表示文档里至少有一个该区域键。
func (c *Client) cachedEntry() (rec *identityRecord, present bool) {
	realms := oaMap(c.readIdentityDoc()["realm"])
	for _, k := range identityRealmKeys {
		v, ok := realms[k]
		if !ok {
			continue
		}
		present = true
		if r := entryToRecord(v); r != nil {
			return r, true
		}
	}
	return nil, present
}

func (c *Client) identityExpired(rec *identityRecord, now time.Time) bool {
	ttl := c.identity.diskTTL
	if ttl <= 0 {
		return false
	}
	return now.Sub(time.Unix(int64(rec.CachedAt), 0)) >= ttl
}

// loadIdentityCache 读落盘缓存；无 / 损坏 / 字段不全 / （非 allowExpired 时）过期返回 nil。
func (c *Client) loadIdentityCache(now time.Time, allowExpired bool) *identityRecord {
	rec, present := c.cachedEntry()
	if rec == nil {
		if present && c.identity.once("cache-fields") {
			c.logf("[machine-identity] WARN: machine identity cache unreadable (missing fields); regenerating")
		}
		return nil
	}
	if !allowExpired && c.identityExpired(rec, now) {
		return nil
	}
	return rec
}

// identityCacheExpired 落盘存在有效条目但已按 TTL 过期（定期轮换路径专用）。
func (c *Client) identityCacheExpired(now time.Time) bool {
	rec, _ := c.cachedEntry()
	return rec != nil && c.identityExpired(rec, now)
}

func sameIdentity(a, b *identityRecord) bool {
	return a.MachineToken == b.MachineToken && a.MachineType == b.MachineType && a.MachineCode == b.MachineCode &&
		a.VM == b.VM && a.Source == b.Source && reflect.DeepEqual(a.VMInfo, b.VMInfo)
}

// saveIdentityCache 写落盘缓存（读-改-写 + 原子替换 + 0600）。
// 返回 nil = 采纳新值；返回记录 = 以磁盘既有值为准（并发防御：另一实例刚写入了未过期且不同的身份）。
// 写盘失败只记日志（身份仍可用于本进程），不向上抛。
func (c *Client) saveIdentityCache(rec *identityRecord, force bool, now time.Time) *identityRecord {
	doc := c.readIdentityDoc()
	realms := oaMap(doc["realm"])
	var old *identityRecord
	for _, k := range identityRealmKeys {
		if r := entryToRecord(realms[k]); r != nil {
			old = r
			break
		}
	}
	if !force && old != nil && !c.identityExpired(old, now) && !sameIdentity(old, rec) {
		if c.identity.once("cache-diff") {
			c.logf("[machine-identity] INFO: identity cache differs from fresh component output; keeping cached")
		}
		return old
	}
	entry := *rec
	entry.CachedAt = float64(now.UnixNano()) / 1e9
	rec.CachedAt = entry.CachedAt
	for _, k := range identityRealmKeys {
		realms[k] = entry
	}
	doc["realm"] = realms
	doc["version"] = 1
	if err := identityWriteJSON(c.identityCachePath(), doc, 0o600); err != nil {
		c.logf("[machine-identity] WARN: could not write machine identity cache: %v", err)
	} else if force && old != nil {
		c.logf("[machine-identity] WARN: machine identity refreshed after upstream rejection (cache updated)")
	}
	return nil
}

// identityWriteJSON JSON 原子写（临时文件 + rename）。
func identityWriteJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, perm)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// purgeIdentitySeed 删除组件身份种子 ~/.config/.locale_cfg（缺失忽略、失败静默）。
func (c *Client) purgeIdentitySeed() bool {
	p := ""
	if c.identity.seedPath != nil {
		p = c.identity.seedPath()
	} else if home, err := os.UserHomeDir(); err == nil {
		p = filepath.Join(home, ".config", ".locale_cfg")
	}
	if p == "" {
		return false
	}
	return os.Remove(p) == nil
}
