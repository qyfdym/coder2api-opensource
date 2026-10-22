package qoder

// OAuth 设备授权（PKCE S256）与 PAT 交换。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：_make_pkce / AccountPool.start_login / poll_login / import_pat /
// _device_expiry。
//
// 设备授权流程：StartLogin 生成 PKCE verifier/challenge 与 nonce，拼出官方
// /device/selectAccounts 授权链接让用户在浏览器打开；之后调用方定期 PollLogin，
// 服务端在用户完成授权前回 404/202（继续等），完成后回 dt- access token（约 30 天）
// 与 drt- refresh token（约 1 年）。
//
// 与原实现的结构差异：原实现把登录会话存进进程内的 logins 表（按 state 查）；本包不做全局表，
// LoginSession 整体交给调用方保存——网关可能多实例 / 重启，会话放在调用方的存储里更可控。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LoginTTL 设备授权会话有效期（原实现 LOGIN_TTL_SECONDS=600）。
const LoginTTL = 600 * time.Second

// pkceAlphabet RFC 7636 unreserved 字符集（66 个）。verifier 每字节取 b % 66，与原实现逐字符一致。
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// pkcePollUA 设备授权轮询的 UA（原实现特意用 "QoderWork"，与账号接口的区域 UA 不同）。
const pkcePollUA = "QoderWork"

// LoginSession 一次设备授权会话。**调用方必须原样保存全部导出字段**直到 PollLogin 返回
// ok / expired / error：Verifier 与 Nonce 是轮询换 token 的凭据（丢了就只能重新开始），
// 其余字段用于展示与过期判断。会话本身不含任何账号凭证，但 Verifier 属于一次性秘密，
// 不要打进日志或回给浏览器端。
type LoginSession struct {
	State     string    // 轮询用的标识（"qd-<纳秒时间戳>"，与原实现同形）
	AuthURL   string    // 让用户在浏览器打开的授权链接
	Realm     string    // cn / intl
	CreatedAt time.Time // 会话创建时间
	ExpiresAt time.Time // CreatedAt + LoginTTL
	Verifier  string    // PKCE code_verifier（64 字符；轮询时作为 verifier 参数）
	Challenge string    // PKCE code_challenge（S256，base64url 无填充）
	Nonce     string    // 国内版为带横线 uuid，国际版为 32 位 hex
	MachineID string    // 授权链接里的 machine_id（随机 uuid；见 StartLogin 说明）
}

// LoginResult 一次轮询的结论。Status 字面量与原实现 poll_login 一致：
//
//	pending —— 用户尚未完成授权（或暂时网络错误），继续轮询；
//	ok      —— 拿到 token，Tokens / User 有效，调用方据此建账号并落盘；
//	expired —— 超过 LoginTTL，需重新 StartLogin；
//	error   —— 上游明确拒绝（非 404/202 的 HTTP 错误），Message 带原因。
type LoginResult struct {
	Status  string
	Message string
	Tokens  TokenSet
	User    UserInfo
}

// pkceFromRandom 由随机字节构造 (verifier, challenge)。拆出来便于用固定输入做向量测试。
func pkceFromRandom(raw []byte) (verifier, challenge string) {
	b := make([]byte, len(raw))
	for i, x := range raw {
		b[i] = pkceAlphabet[int(x)%len(pkceAlphabet)]
	}
	sum := sha256.Sum256(b)
	return string(b), base64.RawURLEncoding.EncodeToString(sum[:])
}

// buildLoginSession 纯函数：给定随机源产物拼出会话与授权链接。
//
// 双区 URL 参数差异（逆向官方客户端，参数顺序与原实现 urlencode 的插入顺序一致）：
//
//	国内版：challenge, challenge_method, nonce(带横线), redirect_uri, client_id, machine_id
//	国际版：challenge, challenge_method, nonce(32-hex), client_id, machine_id（不带 redirect_uri）
//
// 这里刻意不用 url.Values.Encode()：它会按键名排序，与官方 / 原实现的参数顺序不一致。
// url.QueryEscape 与 Python quote_plus 的保留字符集相同（字母数字与 -_.~），空格都转 '+'。
func buildLoginSession(realm string, raw []byte, nonceUUID, machineID string, now time.Time) *LoginSession {
	realm = NormalizeRealm(realm)
	cfg := ConfigFor(realm)
	verifier, challenge := pkceFromRandom(raw)
	nonce := nonceUUID
	if !cfg.NonceDashed {
		nonce = strings.ReplaceAll(nonceUUID, "-", "")
	}
	q := []string{
		"challenge=" + url.QueryEscape(challenge),
		"challenge_method=S256",
		"nonce=" + url.QueryEscape(nonce),
	}
	if cfg.SendRedirectURI {
		q = append(q, "redirect_uri="+url.QueryEscape(cfg.RedirectURI))
	}
	if cfg.SendClientID {
		q = append(q, "client_id="+url.QueryEscape(cfg.ClientID), "machine_id="+url.QueryEscape(machineID))
	}
	return &LoginSession{
		State:     fmt.Sprintf("qd-%d", now.UnixNano()),
		AuthURL:   cfg.Website + "/device/selectAccounts?" + strings.Join(q, "&"),
		Realm:     realm,
		CreatedAt: now,
		ExpiresAt: now.Add(LoginTTL),
		Verifier:  verifier,
		Challenge: challenge,
		Nonce:     nonce,
		MachineID: machineID,
	}
}

// StartLogin 构造一次设备授权会话（不发网络请求）。
//
// machine_id：原实现优先读本机官方客户端的 ~/.qoder[-cn]/.auth/machine_id（保持与本机桌面端
// 同一设备），读不到才生成随机 uuid。网关跑在服务器上、且本包不做本机凭证 / 客户端目录扫描，
// 所以这里固定走「随机 uuid」分支；值保存在 LoginSession.MachineID 里，调用方需要时可记录。
func (c *Client) StartLogin(realm string) (*LoginSession, error) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("qoder login: 生成 PKCE 随机数失败: %w", err)
	}
	return buildLoginSession(realm, raw, RandomUUID(), RandomUUID(), time.Now()), nil
}

// pollURL 轮询地址（参数顺序 nonce, verifier, challenge_method 与原实现一致）。
func (s *LoginSession) pollURL() string {
	return oaURL(s.Realm, oaPathDevicePoll) + "?nonce=" + url.QueryEscape(s.Nonce) +
		"&verifier=" + url.QueryEscape(s.Verifier) + "&challenge_method=S256"
}

// PollLogin 轮询一次设备授权结果。
//
// 返回的 error 只表示调用方用法错误（会话为空 / 缺 verifier）；上游的各种状态都编码在
// LoginResult.Status 里。轮询本身**不带账号凭证**（只有 nonce / verifier），网络错误按原实现
// 归为 pending（下一轮再试），而不是让整个登录失败。
//
// 拿到 token 后会尽力拉一次 userinfo 补全 UID / 昵称 / 用户类型 / 组织；失败不阻塞登录，
// 缺省值与原实现一致（uid 缺失时生成 "q-<24 位 hex>"，昵称缺失时为 "u" + uid 末 8 位）。
func (c *Client) PollLogin(ctx context.Context, s *LoginSession) (LoginResult, error) {
	if s == nil || s.Verifier == "" || s.Nonce == "" {
		return LoginResult{Status: "error", Message: "login session missing - start the login again"},
			fmt.Errorf("qoder login: 会话为空或缺少 verifier/nonce")
	}
	if !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt) {
		return LoginResult{Status: "expired", Message: "login window expired - start again"}, nil
	}
	realm := NormalizeRealm(s.Realm)
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", pkcePollUA)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: s.pollURL(), header: h,
		attempts: 1, timeout: 20 * time.Second})
	if err != nil {
		if resp == nil {
			return LoginResult{Status: "pending", Message: "poll error: " + err.Msg}, nil
		}
		if resp.status == http.StatusNotFound || resp.status == http.StatusAccepted {
			return LoginResult{Status: "pending", Message: "等待浏览器完成 Qoder 设备授权"}, nil
		}
		return LoginResult{Status: "error",
			Message: fmt.Sprintf("poll http %d %s", resp.status, oaCut(string(resp.body), 160))}, nil
	}
	if resp.status == http.StatusAccepted {
		return LoginResult{Status: "pending", Message: "等待浏览器完成 Qoder 设备授权"}, nil
	}
	data, ok := oaObject(resp.body)
	if !ok {
		return LoginResult{Status: "pending", Message: "waiting for grant"}, nil
	}
	token := oaStr(data["token"])
	if token == "" {
		token = oaStr(data["device_token"])
	}
	if token == "" {
		return LoginResult{Status: "pending", Message: "waiting for token"}, nil
	}
	now := time.Now()
	res := LoginResult{
		Status: "ok",
		Tokens: TokenSet{
			AccessToken:  token,
			RefreshToken: oaStr(data["refresh_token"]),
			ExpiresAt:    deviceExpiry(data, now),
		},
	}
	uid := oaStr(data["user_id"])
	info, uerr := c.userInfoWithToken(ctx, realm, token, 1, 15*time.Second)
	if uerr == nil {
		if info.UID == "" {
			info.UID = uid
		}
	} else {
		info = UserInfo{UID: uid, UserType: DefaultUserType, Realm: realm, Domain: ConfigFor(realm).Domain}
	}
	if info.Nickname == "" {
		info.Nickname = "u" + oaTail(info.UID, 8)
	}
	if info.UID == "" {
		info.UID = "q-" + RandomHex(12)
	}
	res.User = info
	res.Message = "登录成功"
	if uerr != nil {
		res.Message = "登录成功（userinfo 拉取失败：" + uerr.Msg + "；昵称 / 组织信息待下次补全）"
	}
	return res, nil
}

// ExchangePAT 用 pt- 个人访问令牌换取 jobToken（jt- access token 24 小时 + jrt- refresh 48 小时）。
//
// 原实现 import_pat 在交换后顺手拉 userinfo 并入库；这里只做交换，调用方随后用新 token
// 调 UserInfo 补全身份（UID 缺失时原实现生成 "p-<24 位 hex>"），并把 PAT 本身存进 Cred.PAT——
// PAT 是 jrt- 过期后的最终兜底（见 Refresh）。
func (c *Client) ExchangePAT(ctx context.Context, realm, pat string) (TokenSet, *Error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return TokenSet{}, &Error{Kind: KindBadRequest, Code: "INVALID_PAT", Msg: "PAT must start with pt-"}
	}
	realm = NormalizeRealm(realm)
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodPost, url: oaURL(realm, oaPathJobExchange),
		body: body, header: oaTokenHeaders(realm, ""), attempts: 3, timeout: 30 * time.Second})
	if err != nil {
		return TokenSet{}, err
	}
	data, _ := oaObject(resp.body)
	token := oaStr(data["token"])
	if token == "" {
		return TokenSet{}, oaNoTokenError("jobToken exchange returned no token", resp.body)
	}
	now := time.Now()
	exp := now.Unix() + 24*3600
	if oaTruthy(data["expires_in"]) {
		exp = now.Unix() + oaInt(data["expires_in"])/1000
	}
	return TokenSet{AccessToken: token, RefreshToken: oaStr(data["refresh_token"]), ExpiresAt: exp}, nil
}

// oaNoTokenError 2xx 但没给 token：多半是业务层拒绝。体里带会话死亡标记时按会话死亡处理
// （原实现在这条路径只记 "returned no token"；这里多做一步分类，便于调用方直接停用账号）。
func oaNoTokenError(msg string, body []byte) *Error {
	e := &Error{Kind: KindServer, Code: "NO_TOKEN", Msg: msg, Body: truncate(string(body), oaBodyKeep)}
	if oaSessionDead(string(body)) {
		e.Kind = KindSessionDead
		if c := oaBusinessCode(body); c != "" {
			e.Code = c
		}
	}
	return e
}

// deviceExpiry deviceToken 响应的到期时间（原实现 _device_expiry）：
// expires_in（毫秒）优先；其次 expires_at（RFC3339）；都没有按 30 天。
//
// expires_at 的解析比原实现多一步：先按带时区的 RFC3339 完整解析；只有失败时才退回原实现的
// 「取前 19 个字符按本地时区解释」——原实现丢弃时区后缀，容器 TZ 与服务端不同时会偏移数小时。
func deviceExpiry(data map[string]any, now time.Time) int64 {
	if oaTruthy(data["expires_in"]) {
		return now.Unix() + oaInt(data["expires_in"])/1000
	}
	if s := oaStr(data["expires_at"]); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.Unix()
		}
		if len(s) >= 19 {
			if t, err := time.ParseInLocation("2006-01-02T15:04:05", s[:19], time.Local); err == nil {
				return t.Unix()
			}
		}
	}
	return now.Unix() + 30*86400
}

// oaTail 取字符串末 n 个字符（不足则整串）。
func oaTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
