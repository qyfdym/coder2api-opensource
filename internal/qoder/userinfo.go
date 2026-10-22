package qoder

// 账号身份查询（GET /api/v1/userinfo）。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：poll_login / import_pat 中拉取 userinfo 的片段。
//
// userinfo 决定 COSY 身份体里的 uid / name / user_type / organization_*（见 sign.go 的 9 键
// 身份体）：这些字段错了，推理请求会被服务端按「身份与 token 不符」拒绝，所以登录 / 导入后
// 必须先补全再入池；团队账号的 organization_id 还用于共享额度跨账号去重（见 credits.go）。

import (
	"context"
	"net/http"
	"time"
)

// UserInfo 账号身份。UID / Nickname / UserType / OrgID / OrgName 对应原实现入库的
// uid / nickname / userType / organizationId / organizationName；Domain / Realm 按所属区域给出。
type UserInfo struct {
	UID      string // id
	Nickname string // name
	Email    string // email（上游有则给出，原实现未使用）
	UserType string // user_type；缺失时为 DefaultUserType（与原实现一致）
	OrgID    string // organization_id（团队 / 企业账号）
	OrgName  string // organization_name
	Domain   string // 区域域名（qoder.com.cn / qoder.com）
	Realm    string // cn / intl
	Avatar   string // avatar / avatar_url（上游有则给出）
}

// UserInfo 用凭证的 access token 查询账号身份（只读）。
func (c *Client) UserInfo(ctx context.Context, cred Cred) (UserInfo, *Error) {
	return c.userInfoWithToken(ctx, cred.Realm, cred.AccessToken, 2, 15*time.Second)
}

// userInfoWithToken 原实现两处调用口径：import_pat 用 retries=2，poll_login 用 retries=1；超时都是 15s。
// 请求头只有 Accept / 区域 UA / Authorization（原实现这里没有用完整的 headers()）。
func (c *Client) userInfoWithToken(ctx context.Context, realm, token string, attempts int, timeout time.Duration) (UserInfo, *Error) {
	realm = NormalizeRealm(realm)
	if token == "" {
		return UserInfo{}, &Error{Kind: KindAuth, Code: "NO_TOKEN", Msg: "empty access token"}
	}
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", ConfigFor(realm).UA)
	h.Set("Authorization", "Bearer "+token)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodGet, url: oaURL(realm, oaPathUserInfo),
		header: h, attempts: attempts, timeout: timeout})
	if err != nil {
		return UserInfo{}, err
	}
	m, ok := oaObject(resp.body)
	if !ok {
		return UserInfo{}, &Error{Kind: KindServer, Status: resp.status, Msg: "userinfo: unexpected body",
			Body: truncate(string(resp.body), oaBodyKeep)}
	}
	return parseUserInfo(m, realm), nil
}

// parseUserInfo 字段映射（纯函数，便于测试）。
func parseUserInfo(m map[string]any, realm string) UserInfo {
	u := UserInfo{
		UID:      oaStr(m["id"]),
		Nickname: oaStr(m["name"]),
		Email:    oaStr(m["email"]),
		UserType: oaStr(m["user_type"]),
		OrgID:    oaStr(m["organization_id"]),
		OrgName:  oaStr(m["organization_name"]),
		Domain:   ConfigFor(realm).Domain,
		Realm:    NormalizeRealm(realm),
		Avatar:   oaStr(m["avatar"]),
	}
	if u.Avatar == "" {
		u.Avatar = oaStr(m["avatar_url"])
	}
	if u.UserType == "" {
		u.UserType = DefaultUserType
	}
	return u
}
