package kiro

// SSO token 导入：用 AWS 门户的登录会话（浏览器 cookie x-amz-sso_authn 的值，即 view.awsapps.com/start
// 登录后的 Bearer）冒充「已在浏览器里批准设备授权」，直接换出 Builder ID 的 access / refresh token，
// 省掉人工点确认。移植自 Kiro-Go auth/sso_token.go：
//
//	1. POST oidc/client/register              注册公共 OIDC 客户端
//	2. POST oidc/device_authorization         发起设备授权，拿 deviceCode / userCode
//	3. GET  portal/token/whoAmI               校验 SSO token
//	4. POST portal/session/device             换设备会话令牌
//	5. POST oidc/device_authorization/accept_user_code   接受 userCode
//	6. POST oidc/device_authorization/associate_token    批准授权（有 deviceContext 时）
//	7. POST oidc/token                        轮询换取 access / refresh token
//
// SSO token 是一份活的门户会话：不写日志、不进错误信息；换出的 OIDC 客户端密钥只留在服务端。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	ssoPortalBase    = "https://portal.sso.us-east-1.amazonaws.com"
	ssoReferer       = "https://view.awsapps.com/"
	ssoPollDeadline  = 2 * time.Minute
	ssoMaxTokenBytes = 16 << 10
)

// SSOTokenResult 一个 SSO token 换出的凭证与它对应的 OIDC 客户端（刷新要用）。
type SSOTokenResult struct {
	Tokens       TokenSet
	ClientID     string
	ClientSecret string
	Region       string
}

// ImportFromSSOToken 执行上面的 7 步。region 为空 = us-east-1。
func (c *Client) ImportFromSSOToken(ctx context.Context, ssoToken, region string) (SSOTokenResult, *Error) {
	ssoToken = strings.TrimSpace(ssoToken)
	if ssoToken == "" {
		return SSOTokenResult{}, &Error{Kind: KindBadRequest, Msg: "SSO token 为空"}
	}
	if len(ssoToken) > ssoMaxTokenBytes || strings.ContainsAny(ssoToken, " \t\r\n\"") {
		return SSOTokenResult{}, &Error{Kind: KindBadRequest, Msg: "SSO token 格式不对（应是 x-amz-sso_authn 这个 cookie 的值，不含空格和引号）"}
	}
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = defaultRegion
	}
	if !regionPattern.MatchString(region) {
		return SSOTokenResult{}, &Error{Kind: KindBadRequest, Msg: fmt.Sprintf("区域 %q 不合法（例如 us-east-1）", region)}
	}
	base := c.oidcBase(region)
	step := func(name string, status int, raw []byte, err error) *Error {
		if err != nil {
			e := transportError(err)
			e.Msg = name + "：" + e.Msg
			return e
		}
		if status != http.StatusOK {
			return &Error{Kind: classify(status, string(raw)), Status: status, Msg: name + "失败", Body: truncate(string(raw), 300)}
		}
		return nil
	}

	// 1. 注册客户端
	status, raw, err := c.postJSON(ctx, base+"/client/register", map[string]any{
		"clientName": "Kiro API Proxy", "clientType": "public", "scopes": oidcScopes,
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"issuerUrl":  BuilderIDStartURL,
	})
	if e := step("注册 OIDC 客户端", status, raw, err); e != nil {
		return SSOTokenResult{}, e
	}
	var reg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if json.Unmarshal(raw, &reg) != nil || reg.ClientID == "" {
		return SSOTokenResult{}, &Error{Kind: KindServer, Msg: "注册 OIDC 客户端：响应缺少 clientId"}
	}

	// 2. 发起设备授权
	status, raw, err = c.postJSON(ctx, base+"/device_authorization", map[string]string{
		"clientId": reg.ClientID, "clientSecret": reg.ClientSecret, "startUrl": BuilderIDStartURL,
	})
	if e := step("发起设备授权", status, raw, err); e != nil {
		return SSOTokenResult{}, e
	}
	var da struct {
		DeviceCode string `json:"deviceCode"`
		UserCode   string `json:"userCode"`
		Interval   int    `json:"interval"`
	}
	if json.Unmarshal(raw, &da) != nil || da.DeviceCode == "" || da.UserCode == "" {
		return SSOTokenResult{}, &Error{Kind: KindServer, Msg: "发起设备授权：响应缺少 deviceCode / userCode"}
	}
	if da.Interval < 1 {
		da.Interval = 1
	}
	if da.Interval > 10 {
		da.Interval = 10
	}

	// 3. 校验 SSO token
	status, raw, err = c.ssoDo(ctx, http.MethodGet, ssoPortalBase+"/token/whoAmI", ssoToken, "", nil)
	if err != nil {
		return SSOTokenResult{}, step("校验 SSO token", 0, nil, err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return SSOTokenResult{}, &Error{Kind: KindSessionDead, Status: status,
			Msg: "SSO token 无效或已过期：请重新登录 view.awsapps.com/start，再复制 x-amz-sso_authn"}
	}
	if e := step("校验 SSO token", status, raw, nil); e != nil {
		return SSOTokenResult{}, e
	}

	// 4. 设备会话令牌
	status, raw, err = c.ssoDo(ctx, http.MethodPost, ssoPortalBase+"/session/device", ssoToken, "", map[string]any{})
	if e := step("获取设备会话", status, raw, err); e != nil {
		return SSOTokenResult{}, e
	}
	var sess struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &sess) != nil || sess.Token == "" {
		return SSOTokenResult{}, &Error{Kind: KindServer, Msg: "获取设备会话：响应缺少 token"}
	}

	// 5. 接受 userCode
	status, raw, err = c.ssoDo(ctx, http.MethodPost, base+"/device_authorization/accept_user_code", "", ssoReferer,
		map[string]string{"userCode": da.UserCode, "userSessionId": sess.Token})
	if e := step("接受用户代码", status, raw, err); e != nil {
		return SSOTokenResult{}, e
	}
	var acc struct {
		DeviceContext *struct {
			DeviceContextID string `json:"deviceContextId"`
			ClientID        string `json:"clientId"`
			ClientType      string `json:"clientType"`
		} `json:"deviceContext"`
	}
	_ = json.Unmarshal(raw, &acc)

	// 6. 批准授权
	if dc := acc.DeviceContext; dc != nil {
		status, raw, err = c.ssoDo(ctx, http.MethodPost, base+"/device_authorization/associate_token", "", ssoReferer, map[string]any{
			"deviceContext": map[string]string{"deviceContextId": dc.DeviceContextID, "clientId": dc.ClientID, "clientType": dc.ClientType},
			"userSessionId": sess.Token,
		})
		if e := step("批准授权", status, raw, err); e != nil {
			return SSOTokenResult{}, e
		}
	}

	// 7. 轮询换 token
	pctx, cancel := context.WithTimeout(ctx, ssoPollDeadline)
	defer cancel()
	interval := time.Duration(da.Interval) * time.Second
	for {
		if werr := c.wait(pctx, interval); werr != nil {
			return SSOTokenResult{}, &Error{Kind: KindTransport, Msg: "等待授权结果超时（2 分钟）"}
		}
		status, raw, err = c.postJSON(pctx, base+"/token", map[string]string{
			"clientId": reg.ClientID, "clientSecret": reg.ClientSecret,
			"grantType": "urn:ietf:params:oauth:grant-type:device_code", "deviceCode": da.DeviceCode,
		})
		if err != nil {
			continue // 瞬时网络错误：下一轮再试，直到总超时
		}
		if status == http.StatusOK {
			var t struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresIn    int64  `json:"expiresIn"`
			}
			if json.Unmarshal(raw, &t) != nil || t.AccessToken == "" || t.RefreshToken == "" {
				return SSOTokenResult{}, &Error{Kind: KindServer, Msg: "换取 token：响应缺少 accessToken / refreshToken"}
			}
			if t.ExpiresIn <= 0 {
				t.ExpiresIn = 3600
			}
			return SSOTokenResult{
				Tokens:   TokenSet{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, ExpiresAt: time.Now().Unix() + t.ExpiresIn},
				ClientID: reg.ClientID, ClientSecret: reg.ClientSecret, Region: region,
			}, nil
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		switch e.Error {
		case "authorization_pending", "AuthorizationPendingException":
			continue
		case "slow_down", "SlowDownException":
			interval += 5 * time.Second
			continue
		}
		return SSOTokenResult{}, &Error{Kind: classify(status, string(raw)), Status: status, Msg: "换取 token 失败：" + firstNonEmptyStr(e.Error, fmt.Sprintf("HTTP %d", status))}
	}
}

// ssoDo 发一个带 Bearer / Referer 的 JSON 请求（不跟随重定向，响应限长）。
func (c *Client) ssoDo(ctx context.Context, method, rawURL, bearer, referer string, body any) (int, []byte, error) {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := c.noRedirect().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := make([]byte, 0, 512)
	buf := make([]byte, 4096)
	for len(out) < maxIdpResponse {
		n, rerr := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, out, nil
}
