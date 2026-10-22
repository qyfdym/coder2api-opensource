package qoder

// 机器身份（官方 UMID 组件 runtime-info）的进程内状态与缓存。
//
// 本文件只引用标准库类型与本文件内定义的类型（lead 约定：client.go 依赖它的零值构造，
// 而方法实现集中在 identity.go）。

import (
	"context"
	"sync"
	"time"
)

// identityRecord 一份机器身份记录。内存缓存与落盘缓存（DataDir/machine_identity.json 的
// realm.<cn|intl> 条目）共用同一形态，JSON 键名与原实现落盘格式逐字一致——两边的缓存文件可以互换。
type identityRecord struct {
	MachineToken string         `json:"machineToken"`
	MachineType  string         `json:"machineType"`
	MachineCode  string         `json:"machineCode"`
	VM           bool           `json:"vm"`
	VMInfo       map[string]any `json:"vm_info"`
	Source       string         `json:"source"`
	CachedAt     float64        `json:"cached_at,omitempty"` // Unix 秒（浮点，与原实现一致）
}

// identityState 进程内状态。
//
// mu 串行化整个身份解析过程：组件一次调用约 3.7 秒、首次表决要跑 3~5 次，并发调用方若各自
// 拉起组件既浪费又可能各拿到不同分支的样本，所以宁可让后来者排队等同一个结论。
type identityState struct {
	mu sync.Mutex

	// 内存缓存（原实现 _native_ident_cache，TTL 1800s）。
	mem      *identityRecord
	memAt    time.Time
	memPath  string // 产出 / 验证该身份时使用的组件路径（落盘命中时可能为空）
	memStale bool   // 组件当前不可用，回退到了落盘缓存（哪怕过期）
	memDisk  bool   // 来自落盘缓存（未调用组件）

	// 负缓存：组件存在但执行失败时，TTL 内不再反复拉起（原实现缓存空结果 1800s）。
	// 组件「不存在」不做负缓存——探测只是几次 stat，便于调用方装好组件后无需重启即生效。
	failErr error
	failAt  time.Time

	// 一次性日志标记（同类事件进程内只打一次，避免批量签到时刷屏）。
	warned map[string]bool

	// 参数（缺省值见 newIdentityState；测试可直接改写）。
	memTTL     time.Duration // 内存缓存 TTL（原实现 NATIVE_IDENTITY_TTL=1800s）
	diskTTL    time.Duration // 落盘缓存 TTL；0 = 永不过期（原实现 QD_MACHINE_IDENTITY_CACHE_TTL 缺省）
	vote       bool          // 首次落盘前多数表决（原实现 QD_MACHINE_IDENTITY_VOTE 缺省开启）
	voteRounds int           // 首轮采样次数（IDENTITY_VOTE_ROUNDS=3）
	voteExtend int           // 出现分歧时补投次数（IDENTITY_VOTE_EXTEND_ROUNDS=2）
	runTimeout time.Duration // 单次组件调用超时（原实现 25s）

	// 可注入点（测试用；nil = 生产实现）。
	run         func(ctx context.Context, exe string, stdin []byte) ([]byte, error)
	seedPath    func() string   // 组件身份种子文件路径（缺省 ~/.config/.locale_cfg）
	installDirs func() []string // Windows 官方桌面客户端安装目录候选
	now         func() time.Time
}

func newIdentityState() *identityState {
	return &identityState{
		warned:     map[string]bool{},
		memTTL:     1800 * time.Second,
		vote:       true,
		voteRounds: 3,
		voteExtend: 2,
		runTimeout: 25 * time.Second,
	}
}

// clock 当前时间（可注入）。
func (s *identityState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// once 同一 key 进程内只返回一次 true（调用方持有 mu）。
func (s *identityState) once(key string) bool {
	if s.warned == nil {
		s.warned = map[string]bool{}
	}
	if s.warned[key] {
		return false
	}
	s.warned[key] = true
	return true
}
