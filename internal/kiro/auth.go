package kiro

// token 刷新与设备授权登录（移植自 Kiro-Go auth/oidc.go、auth/builderid.go）。
//
//   - social（Google / GitHub 社交登录）：POST prod.us-east-1.auth.desktop.kiro.dev/refreshToken；
//   - idc（AWS Builder ID / IAM Identity Center）：POST oidc.<region>.amazonaws.com/token，
//     需要登录时注册的 OIDC 客户端（clientId / clientSecret）；
//   - external_idp（Microsoft 企业 SSO）：见 externalidp.go，表单提交到租户 token 端点；
//   - api_key：不刷新。
//
// 设备授权（Builder ID 与 IAM Identity Center 共用一条流程，后者多一个 startUrl）：
// 注册 OIDC 客户端 → device_authorization 拿到 userCode + 验证链接 → 用户在浏览器里
// 用 AWS 账号（邮箱 + 密码）登录并确认 → 轮询 /token 拿到 access / refresh token。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BuilderIDStartURL AWS Builder ID 的起始地址（IAM Identity Center 用组织自己的地址）。
const BuilderIDStartURL = "https://view.awsapps.com/start"

var oidcScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

func (c *Client) oidcBase(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if !regionPattern.MatchString(region) {
		region = defaultRegion
	}
	return "https://oidc." + region + ".amazonaws.com"
}

const socialRefreshURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"

// postJSON 发一个 JSON POST，返回状态码与响应体（传输错误返回 err）。
func (c *Client) postJSON(ctx context.Context, rawURL string, body any) (int, []byte, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.auth.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// Refresh 用 refresh token 换新的 access token。
func (c *Client) Refresh(ctx context.Context, cred Cred) (TokenSet, *Error) {
	if cred.IsAPIKey() {
		return TokenSet{}, &Error{Kind: KindBadRequest, Msg: "API Key 账号不需要刷新"}
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return TokenSet{}, &Error{Kind: KindSessionDead, Msg: "账号没有 refresh token，需要重新登录"}
	}
	var (
		target string
		body   any
	)
	switch strings.ToLower(strings.TrimSpace(cred.AuthMethod)) {
	case "social":
		target, body = socialRefreshURL, map[string]string{"refreshToken": cred.RefreshToken}
	case AuthMethodExternalIDP:
		return c.refreshExternalIdp(ctx, cred)
	default:
		if cred.ClientID == "" || cred.ClientSecret == "" {
			return TokenSet{}, &Error{Kind: KindSessionDead, Msg: "IdC 账号缺少 clientId / clientSecret，需要重新登录"}
		}
		target = c.oidcBase(cred.Region) + "/token"
		body = map[string]string{
			"clientId": cred.ClientID, "clientSecret": cred.ClientSecret,
			"refreshToken": cred.RefreshToken, "grantType": "refresh_token",
		}
	}
	status, raw, err := c.postJSON(ctx, target, body)
	if err != nil {
		return TokenSet{}, transportError(err)
	}
	if status != http.StatusOK {
		kind := classifyRefresh(status, string(raw))
		return TokenSet{}, &Error{Kind: kind, Status: status, Code: errorCode(string(raw)), Msg: "刷新 token 失败：" + friendlyMessage(kind), Body: truncate(string(raw), 1000)}
	}
	var r struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if jerr := json.Unmarshal(raw, &r); jerr != nil || strings.TrimSpace(r.AccessToken) == "" {
		return TokenSet{}, &Error{Kind: KindServer, Status: status, Msg: "刷新响应里没有 accessToken", Body: truncate(string(raw), 300)}
	}
	ts := TokenSet{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ProfileArn: strings.TrimSpace(r.ProfileArn)}
	if ts.RefreshToken == "" {
		ts.RefreshToken = cred.RefreshToken
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 3600
	}
	ts.ExpiresAt = time.Now().Unix() + r.ExpiresIn
	return ts, nil
}

// ---------------------------------------------------------------------------
// 设备授权登录（Builder ID / IAM Identity Center）
// ---------------------------------------------------------------------------

// LoginSession 进行中的设备授权（只保存在服务端内存；浏览器只拿 ID、userCode 与验证链接）。
type LoginSession struct {
	ID              string
	Region          string
	StartURL        string
	Provider        string // BuilderId / Enterprise
	ClientID        string
	ClientSecret    string
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int
	ExpiresAt       time.Time
}

// StartDeviceLogin 发起设备授权。startURL 为空 = AWS Builder ID；否则是 IAM Identity Center
// 的组织起始地址（必须是 https://）。
func (c *Client) StartDeviceLogin(ctx context.Context, startURL, region string) (*LoginSession, *Error) {
	startURL = strings.TrimRight(strings.TrimSpace(startURL), "/")
	provider := "Enterprise"
	if startURL == "" || strings.EqualFold(startURL, BuilderIDStartURL) {
		startURL, provider = BuilderIDStartURL, "BuilderId"
	} else if !strings.HasPrefix(startURL, "https://") {
		return nil, &Error{Kind: KindBadRequest, Msg: "Start URL 必须是 https:// 地址"}
	}
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = defaultRegion
	}
	if !regionPattern.MatchString(region) {
		return nil, &Error{Kind: KindBadRequest, Msg: fmt.Sprintf("区域 %q 不合法（例如 us-east-1）", region)}
	}
	base := c.oidcBase(region)
	status, raw, err := c.postJSON(ctx, base+"/client/register", map[string]any{
		"clientName": "Kiro", "clientType": "public", "scopes": oidcScopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  startURL,
	})
	if err != nil {
		return nil, transportError(err)
	}
	if status != http.StatusOK {
		return nil, &Error{Kind: classify(status, string(raw)), Status: status, Msg: "注册 OIDC 客户端失败", Body: truncate(string(raw), 500)}
	}
	var reg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if json.Unmarshal(raw, &reg) != nil || reg.ClientID == "" {
		return nil, &Error{Kind: KindServer, Msg: "注册 OIDC 客户端：响应缺少 clientId", Body: truncate(string(raw), 300)}
	}
	status, raw, err = c.postJSON(ctx, base+"/device_authorization", map[string]string{
		"clientId": reg.ClientID, "clientSecret": reg.ClientSecret, "startUrl": startURL,
	})
	if err != nil {
		return nil, transportError(err)
	}
	if status != http.StatusOK {
		return nil, &Error{Kind: classify(status, string(raw)), Status: status, Msg: "发起设备授权失败", Body: truncate(string(raw), 500)}
	}
	var da struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expiresIn"`
	}
	if json.Unmarshal(raw, &da) != nil || da.DeviceCode == "" {
		return nil, &Error{Kind: KindServer, Msg: "设备授权：响应缺少 deviceCode", Body: truncate(string(raw), 300)}
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	if da.ExpiresIn <= 0 {
		da.ExpiresIn = 600
	}
	uri := da.VerificationURIComplete
	if uri == "" {
		uri = da.VerificationURI
	}
	return &LoginSession{
		ID: newUUID(), Region: region, StartURL: startURL, Provider: provider,
		ClientID: reg.ClientID, ClientSecret: reg.ClientSecret, DeviceCode: da.DeviceCode,
		UserCode: da.UserCode, VerificationURI: uri, Interval: da.Interval,
		ExpiresAt: time.Now().Add(time.Duration(da.ExpiresIn) * time.Second),
	}, nil
}

// LoginResult 一次轮询的结果。Status：pending / slow_down / ok / expired / denied / error。
type LoginResult struct {
	Status  string
	Message string
	Tokens  TokenSet
}

// PollDeviceLogin 轮询一次授权结果。
func (c *Client) PollDeviceLogin(ctx context.Context, s *LoginSession) LoginResult {
	if s == nil {
		return LoginResult{Status: "expired", Message: "授权会话不存在"}
	}
	if time.Now().After(s.ExpiresAt) {
		return LoginResult{Status: "expired", Message: "授权已过期，请重新发起"}
	}
	status, raw, err := c.postJSON(ctx, c.oidcBase(s.Region)+"/token", map[string]string{
		"clientId": s.ClientID, "clientSecret": s.ClientSecret,
		"grantType": "urn:ietf:params:oauth:grant-type:device_code", "deviceCode": s.DeviceCode,
	})
	if err != nil {
		return LoginResult{Status: "pending", Message: "轮询暂时失败，稍后自动重试"}
	}
	if status == http.StatusOK {
		var r struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		}
		if json.Unmarshal(raw, &r) != nil || r.AccessToken == "" {
			return LoginResult{Status: "error", Message: "授权响应里没有 accessToken"}
		}
		if r.ExpiresIn <= 0 {
			r.ExpiresIn = 3600
		}
		return LoginResult{Status: "ok", Message: "授权成功", Tokens: TokenSet{
			AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: time.Now().Unix() + r.ExpiresIn,
		}}
	}
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Error {
	case "authorization_pending", "AuthorizationPendingException":
		return LoginResult{Status: "pending", Message: "等待在浏览器中完成授权"}
	case "slow_down", "SlowDownException":
		s.Interval += 5
		return LoginResult{Status: "slow_down", Message: "等待在浏览器中完成授权"}
	case "expired_token", "ExpiredTokenException":
		return LoginResult{Status: "expired", Message: "授权码已过期，请重新发起"}
	case "access_denied", "AccessDeniedException":
		return LoginResult{Status: "denied", Message: "授权被拒绝"}
	}
	msg := firstNonEmptyStr(e.ErrorDescription, e.Error, truncate(string(raw), 200))
	return LoginResult{Status: "error", Message: fmt.Sprintf("授权失败（HTTP %d）：%s", status, msg)}
}
