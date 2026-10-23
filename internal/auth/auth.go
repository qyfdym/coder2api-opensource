// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// platform 账号所属平台（落盘于顶层 platform）。空 = WorkBuddy（老凭证零回归）。
	// 统一账号池里 WorkBuddy 与 Qoder 账号共存：选号按 Realm() 返回的「路由域」过滤
	// （WorkBuddy = cn / global，Qoder = qoder-cn / qoder-intl），平台专属的上游调用、
	// 签到与任务体系按 IsQoder() / IsWorkBuddy() 分派。
	platform string
	// qoder Qoder 账号的平台专属字段（PAT、套餐、签到留痕等）；WorkBuddy 账号为 nil。
	// 读写一律经 QoderMeta() / UpdateQoder()（与 token 字段同锁）。
	qoder *QoderMeta
	// kiro Kiro 账号的平台专属字段（认证方式、OIDC 客户端、profileArn 等）；其它平台为 nil。
	// 读写一律经 KiroMeta() / UpdateKiro()（与 token 字段同锁）。
	kiro *KiroMeta
}

// 平台标识。
const (
	PlatformWorkBuddy = "workbuddy"
	PlatformQoder     = "qoder"
	PlatformKiro      = "kiro"
)

// Qoder 账号的路由域（账号池按它过滤候选；也是用量 / 状态里的 realm 取值）。
const (
	RealmQoderCN   = "qoder-cn"
	RealmQoderIntl = "qoder-intl"
)

// RealmKiro Kiro 账号的路由域。Kiro 只有一个对外服务面（数据面区域由账号的 profileArn
// 决定，对网关透明），所以只有一个路由域、没有平台内的两域互退。
const RealmKiro = "kiro"

// IsQoderRealm 报告路由域是否属于 Qoder（qoder-cn / qoder-intl）。
func IsQoderRealm(realm string) bool { return strings.HasPrefix(realm, "qoder-") }

// IsKiroRealm 报告路由域是否属于 Kiro。
func IsKiroRealm(realm string) bool { return realm == RealmKiro }

// KiroMeta Kiro 账号的平台专属字段（落盘于顶层 kiro 对象）。
//
// JSON 键名与 Kiro-Go / Kiro Account Manager 的账号字段一致，扁平形 kiro-*.json
// （直接从这些工具导出的单个账号对象）可以原样放进 auth_dir 载入。
type KiroMeta struct {
	// AuthMethod 认证方式：social（Google / GitHub / 社交登录，走 Kiro 桌面认证刷新）/
	// idc（AWS Builder ID 与 IAM Identity Center，走 OIDC 刷新）/ external_idp（Microsoft 企业
	// SSO，走 Entra 租户端点刷新）/ api_key（ksk_ 密钥，不刷新）。
	AuthMethod string `json:"authMethod,omitempty"`
	// Provider 身份来源：BuilderId / Enterprise / Google / Github / AzureAD / Social / APIKey。
	Provider string `json:"provider,omitempty"`
	// Region OIDC 认证区域；API Key 账号是数据面区域。缺省 us-east-1。
	Region string `json:"region,omitempty"`
	// ClientID / ClientSecret OIDC 客户端（idc 刷新必需；social / api_key 为空）。
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// StartURL IAM Identity Center 的起始地址（Builder ID 为官方默认地址）。
	StartURL string `json:"startUrl,omitempty"`
	// TokenEndpoint / IssuerURL / Scopes 仅 external_idp（Microsoft 企业 SSO）：刷新 token 的
	// Entra 租户端点、签发者与申请的 scope（空格分隔）。其它方式为空。
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
	IssuerURL     string `json:"issuerUrl,omitempty"`
	Scopes        string `json:"scopes,omitempty"`
	// ProfileArn Kiro / CodeWhisperer profile（决定数据面区域；刷新响应或 profile 列表里取得）。
	ProfileArn string `json:"profileArn,omitempty"`
	// MachineID 出站请求头里的机器标识（每号固定，首次建档时生成）。
	MachineID string `json:"machineId,omitempty"`
	// APIKey ksk_ 开头的 Kiro API Key（api_key 账号；与 accessToken 同值）。
	APIKey string `json:"kiroApiKey,omitempty"`
	Email  string `json:"email,omitempty"`
	UserID string `json:"userId,omitempty"`
	// Plan 订阅档位（FREE / PRO / PRO_PLUS / POWER），余额查询时补全。
	Plan      string `json:"subscriptionType,omitempty"`
	PlanTitle string `json:"subscriptionTitle,omitempty"`
	// NextResetAt 额度下次重置时刻（Unix 秒；额度耗尽的冷却以它为准）。
	NextResetAt int64 `json:"nextResetAt,omitempty"`
	// OverageStatus 超额（额度用完后按量计费）开关：ENABLED / DISABLED / 空 = 未知。查额度时顺带
	// 刷新；ENABLED 时额度用完不进硬冷却（上游仍会放行，超出部分计费）。
	OverageStatus string `json:"overageStatus,omitempty"`
	// Source 建档来源：builder_id / idc / import / api_key。
	Source  string `json:"source,omitempty"`
	AddedAt int64  `json:"addedAt,omitempty"`
}

func (m *KiroMeta) clone() *KiroMeta {
	if m == nil {
		return nil
	}
	c := *m
	return &c
}

// QoderMeta Qoder 账号的平台专属字段。
//
// 区域（cn / intl）存在 Auth 的 realm 字段里（与 WorkBuddy 共用同一个落盘键），
// 这里只放 Qoder 独有的东西。字段由 qoderacct 适配层维护，本包只负责序列化。
type QoderMeta struct {
	// PAT 个人访问令牌（pt-），jrt- 刷新失败时的最终兜底；OAuth 账号为空。
	PAT          string                       `json:"personalToken,omitempty"`
	UserType     string                       `json:"userType,omitempty"`
	OrgID        string                       `json:"organizationId,omitempty"`
	OrgName      string                       `json:"organizationName,omitempty"`
	Email        string                       `json:"email,omitempty"`
	Plan         string                       `json:"plan,omitempty"`
	Source       string                       `json:"source,omitempty"` // oauth / pat / import
	AddedAt      int64                        `json:"addedAt,omitempty"`
	LastCheckin  string                       `json:"lastCheckin,omitempty"` // "2006-01-02 15:04:05"（本地时区）
	CheckinNote  string                       `json:"lastCheckinMessage,omitempty"`
	CampaignCode map[string]QoderCampaignCode `json:"campaignCodes,omitempty"`
	// CampaignBlockedUntil campaignId → Unix 秒：「同人已领」后的冷却截止（期内不再重复领取）。
	CampaignBlockedUntil map[string]int64 `json:"campaignBlockedUntil,omitempty"`
}

// QoderCampaignCode 活动领到的兑换码（活动只发一次，必须持久化）。
type QoderCampaignCode struct {
	Campaign string `json:"campaign,omitempty"`
	Code     string `json:"code"`
	At       int64  `json:"at,omitempty"`
}

func (m *QoderMeta) clone() *QoderMeta {
	if m == nil {
		return nil
	}
	c := *m
	if m.CampaignCode != nil {
		c.CampaignCode = make(map[string]QoderCampaignCode, len(m.CampaignCode))
		for k, v := range m.CampaignCode {
			c.CampaignCode[k] = v
		}
	}
	if m.CampaignBlockedUntil != nil {
		c.CampaignBlockedUntil = make(map[string]int64, len(m.CampaignBlockedUntil))
		for k, v := range m.CampaignBlockedUntil {
			c.CampaignBlockedUntil[k] = v
		}
	}
	return &c
}

// NewQoder 构造一个 Qoder 账号（面板登录 / PAT 导入用）。region 取 cn / intl。
func NewQoder(uid, nickname, region, accessToken, refreshToken string, expiresAt int64, meta QoderMeta) *Auth {
	return &Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		realm:        normalizeQoderRegion(region),
		UID:          uid,
		Nickname:     nickname,
		platform:     PlatformQoder,
		qoder:        meta.clone(),
	}
}

func normalizeQoderRegion(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "intl", "global", "international":
		return "intl"
	default:
		return "cn"
	}
}

// NewKiro 构造一个 Kiro 账号（面板登录 / 凭证导入用）。
func NewKiro(uid, nickname, accessToken, refreshToken string, expiresAt int64, meta KiroMeta) *Auth {
	return &Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		UID:          uid,
		Nickname:     nickname,
		platform:     PlatformKiro,
		kiro:         meta.clone(),
	}
}

// Platform 账号平台（workbuddy / qoder / kiro）。
func (a *Auth) Platform() string {
	if a != nil {
		switch a.platform {
		case PlatformQoder:
			return PlatformQoder
		case PlatformKiro:
			return PlatformKiro
		}
	}
	return PlatformWorkBuddy
}

// IsQoder 报告是否为 Qoder 账号。
func (a *Auth) IsQoder() bool { return a != nil && a.platform == PlatformQoder }

// IsKiro 报告是否为 Kiro 账号。
func (a *Auth) IsKiro() bool { return a != nil && a.platform == PlatformKiro }

// IsWorkBuddy 报告是否为 WorkBuddy 账号（含老凭证的空平台）。
// WorkBuddy 专属的体系（签到 / 成长任务 / 旅行 / 补偿 / 活跃上报）一律以它为门控：
// 其它平台的账号不会被误当成 WorkBuddy 号去打 WorkBuddy 端点。
func (a *Auth) IsWorkBuddy() bool {
	return a != nil && a.platform != PlatformQoder && a.platform != PlatformKiro
}

// KiroMeta 返回 Kiro 专属字段的快照（副本）；其它平台返回零值。
func (a *Auth) KiroMeta() KiroMeta {
	if !a.IsKiro() {
		return KiroMeta{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kiro == nil {
		return KiroMeta{}
	}
	return *a.kiro
}

// UpdateKiro 在账号锁内修改 Kiro 专属字段（调用方随后 SaveAtomic 落盘）。
func (a *Auth) UpdateKiro(fn func(m *KiroMeta)) {
	if !a.IsKiro() || fn == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kiro == nil {
		a.kiro = &KiroMeta{}
	}
	fn(a.kiro)
}

// QoderRegion Qoder 账号的区域（cn / intl）；WorkBuddy 账号返回空串。
func (a *Auth) QoderRegion() string {
	if !a.IsQoder() {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return normalizeQoderRegion(a.realm)
}

// QoderMeta 返回 Qoder 专属字段的快照（副本，可放心读）；WorkBuddy 账号返回零值。
func (a *Auth) QoderMeta() QoderMeta {
	if !a.IsQoder() {
		return QoderMeta{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.qoder == nil {
		return QoderMeta{}
	}
	return *a.qoder.clone()
}

// UpdateQoder 在账号锁内修改 Qoder 专属字段（调用方随后 SaveAtomic 落盘）。
func (a *Auth) UpdateQoder(fn func(m *QoderMeta)) {
	if !a.IsQoder() || fn == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.qoder == nil {
		a.qoder = &QoderMeta{}
	}
	fn(a.qoder)
}

// SetTokens 在账号锁内写入刷新后的凭证（非 WorkBuddy 上游的刷新路径用；空 refresh 不覆盖）。
func (a *Auth) SetTokens(accessToken, refreshToken string, expiresAt int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(accessToken) != "" {
		a.AccessToken = accessToken
	}
	if strings.TrimSpace(refreshToken) != "" {
		a.RefreshToken = refreshToken
	}
	a.ExpiresAt = expiresAt
}

// ExpiresAtValue 加锁读取 ExpiresAt。
func (a *Auth) ExpiresAtValue() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	// Qoder 账号的路由域与 WorkBuddy 两域互不重叠，也不受 global 逃生门影响
	// （逃生门只锁 WorkBuddy 的 global 端点）。
	if a.platform == PlatformQoder {
		return "qoder-" + normalizeQoderRegion(a.realm)
	}
	if a.platform == PlatformKiro {
		return RealmKiro
	}
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" || a.platform == PlatformQoder || a.platform == PlatformKiro {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// IsEnterpriseSession 报告当前凭证是否处于「企业上下文」：
// 账号本身是企业版（IsEnterprise），或 AccessToken 签发自**企业切换会话**
// （JWT payload 带 token_source=enterprise_switch——浏览器在企业空间状态下
// 完成 OAuth 授权时签发）。
//
// 为什么单独一站：企业切换会话与真企业版的上游表现**不同**（实测 2026-10-12，
// 同一时刻与个人号 A/B 对照）——成长任务列表 / 额度查询 / 领养 Buddy 等**读端点
// 与直连 API 照常**，但 /v2/report 行为事件**返回 200 却被静默丢弃**（15 个成长
// 任务全部 0/x，重跑并 90s 内逐 15s 回读进度从不跳动），连登 / 签到同样无效。
// 即「读得见、记不上账」：不跳过则队列每轮白跑全部任务（含真实对话配额）且
// 无任何报错。因此行为类任务（活跃上报 / 连登 / 旅行 / 夜猫子 / 成长任务执行）
// 的门控应使用本方法；签到与任务列表扫描不在此列（签到失败是显式报错，
// 列表扫描实测可用）。额度查询 / 选号 / 保活照常（按 IsEnterprise 口径）。
//
// 每次现解码 JWT 而非 Parse 时缓存：RefreshToken 会整体换发 token，
// 企业切换状态可能随重新授权翻转，缓存会读到过期形态。解码成本仅一次
// base64 + 小 JSON（每调度轮每账号数次），可忽略。
func (a *Auth) IsEnterpriseSession() bool {
	if a.IsEnterprise() {
		return true
	}
	return jwtTokenSource(a.AccessTokenValue()) == "enterprise_switch"
}

// jwtTokenSource 解出 JWT payload 中的 token_source 声明；非 JWT / 解码失败
// 一律返回 ""（老 token 与非标准形态按无此声明处理，零回归）。先按无填充
// base64url 解（JWT 标准），失败再试带填充形态（兼容非标准签发方）。
func jwtTokenSource(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var payload struct {
		TokenSource string `json:"token_source"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.TokenSource
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) { return parse(raw, "") }

// parse forcePlatform 非空时无论文件内有无 platform 字段都按该平台解析
// （qoder-*.json → Qoder，kiro-*.json → Kiro，见 LoadDir）。
func parse(raw []byte, forcePlatform string) (*Auth, error) {
	forceQoder := forcePlatform == PlatformQoder
	forceKiro := forcePlatform == PlatformKiro
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string     `json:"device_token"`
			Platform    string     `json:"platform"`
			Qoder       *QoderMeta `json:"qoder"`
			Kiro        *KiroMeta  `json:"kiro"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
		}
		switch {
		case forceKiro || (!forceQoder && strings.EqualFold(strings.TrimSpace(n.Platform), PlatformKiro)):
			a.platform = PlatformKiro
			a.realm = ""
			a.kiro = n.Kiro
			if a.kiro == nil {
				a.kiro = &KiroMeta{}
			}
		case forceQoder || strings.EqualFold(strings.TrimSpace(n.Platform), PlatformQoder):
			a.platform = PlatformQoder
			a.realm = normalizeQoderRegion(n.Auth.Realm)
			a.qoder = n.Qoder
			if a.qoder == nil {
				a.qoder = &QoderMeta{}
			}
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
			// 以下为 Qoder 扁平形（qoder2api-hub 的账号文件）：改名为 qoder-<uid>.json 放进
			// auth_dir 即可直接载入，下次落盘自动转成嵌套形。
			Platform      string          `json:"platform"`
			PersonalToken string          `json:"personalToken"`
			UserType      string          `json:"userType"`
			OrgID         string          `json:"organizationId"`
			OrgName       string          `json:"organizationName"`
			Plan          string          `json:"plan"`
			Source        string          `json:"source"`
			LastCheckin   json.RawMessage `json:"lastCheckin"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
		// 原实现的 platform 字段是客户端形态（"CLI" 等），不是平台标识；Qoder 扁平形靠
		// 文件名（qoder-*.json，见 LoadDir）或显式 platform=qoder 判定。
		// Kiro 扁平形（Kiro-Go / Kiro Account Manager 的单个账号对象）同理靠 kiro-*.json
		// 文件名或显式 platform=kiro：KiroMeta 的键名与这些工具一致，直接整段解码。
		if forceKiro || (!forceQoder && strings.EqualFold(strings.TrimSpace(f.Platform), PlatformKiro)) {
			var m KiroMeta
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("storage_parse_error: %w", err)
			}
			a.platform = PlatformKiro
			a.realm = ""
			a.kiro = &m
			if a.UID == "" {
				// Kiro-Go 的账号对象没有 uid 键（主键叫 id）。
				var idOnly struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(raw, &idOnly)
				a.UID = strings.TrimSpace(idOnly.ID)
			}
			if a.Nickname == "" {
				a.Nickname = m.Email
			}
			// API Key 账号：密钥本身就是 Bearer 凭证。
			if strings.TrimSpace(a.AccessToken) == "" && m.APIKey != "" {
				a.AccessToken = m.APIKey
			}
		} else if forceQoder || strings.EqualFold(strings.TrimSpace(f.Platform), PlatformQoder) {
			a.platform = PlatformQoder
			a.realm = normalizeQoderRegion(f.Realm)
			m := &QoderMeta{
				PAT: f.PersonalToken, UserType: f.UserType, OrgID: f.OrgID, OrgName: f.OrgName,
				Plan: f.Plan, Source: f.Source,
			}
			// lastCheckin 只认 "YYYY-MM-DD ..." 字符串（其它形态丢弃，最多让今天多签一次，幂等无害）。
			var lc string
			if json.Unmarshal(f.LastCheckin, &lc) == nil && len(lc) >= 10 {
				m.LastCheckin = lc
			}
			a.qoder = m
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// Qoder 账号：顶层 platform + qoder 专属字段（WorkBuddy 账号不写，旧文件形状不变）。
	if a.platform == PlatformQoder {
		doc["platform"] = PlatformQoder
		meta := a.qoder
		if meta == nil {
			meta = &QoderMeta{}
		}
		doc["qoder"] = meta
	}
	// Kiro 账号：顶层 platform + kiro 专属字段（含 OIDC clientSecret，文件权限 0600）。
	if a.platform == PlatformKiro {
		doc["platform"] = PlatformKiro
		meta := a.kiro
		if meta == nil {
			meta = &KiroMeta{}
		}
		doc["kiro"] = meta
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// QoderFileName Qoder 账号凭证文件名（与 LoadDir 的 glob 对应）。
func QoderFileName(uid string) string { return "qoder-" + uid + ".json" }

// KiroFileName Kiro 账号凭证文件名（与 LoadDir 的 glob 对应）。
func KiroFileName(uid string) string { return "kiro-" + uid + ".json" }

// filePlatform 按文件名判定平台（qoder*.json / kiro*.json；其余空 = 按文件内容）。
func filePlatform(path string) string {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasPrefix(base, "qoder"):
		return PlatformQoder
	case strings.HasPrefix(base, "kiro"):
		return PlatformKiro
	}
	return ""
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json、qoder*.json 与 kiro*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// Qoder 账号与 WorkBuddy 账号同目录：qoder-<uid>.json（文件内顶层 platform=qoder）。
	qfiles, err := filepath.Glob(filepath.Join(dir, "qoder*.json"))
	if err != nil {
		return nil, err
	}
	files = append(files, qfiles...)
	// Kiro 账号同目录：kiro-<uid>.json（文件内顶层 platform=kiro）。
	kfiles, err := filepath.Glob(filepath.Join(dir, "kiro*.json"))
	if err != nil {
		return nil, err
	}
	files = append(files, kfiles...)
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// 文件名即平台：qoder*.json 一律按 Qoder 账号解析（兼容 qoder2api-hub 的扁平账号文件），
		// kiro*.json 一律按 Kiro 账号解析（兼容 Kiro-Go 导出的单个账号对象）。
		a, err := parse(raw, filePlatform(f))
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
