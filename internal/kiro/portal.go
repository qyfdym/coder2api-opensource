package kiro

// Kiro 网页登录（hosted portal）：Google / GitHub 社交登录与 Microsoft 企业 SSO 共用一个入口
// https://app.kiro.dev/signin。浏览器登录完成后会被重定向到 http://localhost:3128/...，网关机器上
// 并没有进程监听这个端口，页面会显示「无法访问 localhost」——这是预期的：用户把地址栏里的完整 URL
// 粘回面板，服务端从里面取授权码（社交）或企业身份源描述符（Microsoft），不需要开任何监听端口。
//
//	第 1 段（portal）：state / code_challenge 由服务端生成，URL 交给用户在浏览器里打开；
//	  粘回的 URL 带 code → 社交：用 PKCE verifier 换 token；
//	  带 login_option=external_idp&issuer_url&client_id&scopes → 企业 SSO：OIDC 发现 → 第 2 段；
//	第 2 段（Microsoft）：服务端生成新的 PKCE，给出微软授权 URL；粘回 /oauth/callback?code&state，
//	  换 token（表单、公共客户端）。
//
// 社交登录的 token 交换（prod.us-east-1.auth.desktop.kiro.dev/oauth/token）依据公开的第三方实现
// 编写，未经真机验证；企业 SSO 部分移植自 Kiro-Go。PKCE verifier、state、临时凭证只保存在服务端
// 内存里，完成 / 取消 / 过期即清除。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	portalSignInURL    = "https://app.kiro.dev/signin"
	portalRedirectBase = "http://localhost:3128"
	portalRedirectFrom = "KiroIDE"
	portalOAuthPath    = "/oauth/callback"
	portalSessionTTL   = 10 * time.Minute
	portalMaxCallback  = 16 << 10
	socialTokenURL     = "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token"
)

type portalStage int

const (
	portalWaitingPortal portalStage = iota + 1
	portalWaitingProvider
	portalDone
)

type portalProviderLeg struct {
	State         string
	Verifier      string
	IssuerURL     string
	TokenEndpoint string
	ClientID      string
	Scopes        string
}

// PortalSession 一次进行中的网页登录（只存服务端内存）。
type PortalSession struct {
	ID        string
	ExpiresAt time.Time

	mu       sync.Mutex
	stage    portalStage
	state    string
	verifier string
	leg      *portalProviderLeg
}

// PortalResult 登录完成得到的凭证。Kind：social / external_idp。
type PortalResult struct {
	Kind   string
	Tokens TokenSet
	// 仅 external_idp：
	ClientID      string
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	Email         string
	UserID        string
}

// PortalProgress 一次粘贴回调的结果：要么给出下一段的授权 URL，要么给出最终凭证。
type PortalProgress struct {
	AuthorizationURL string
	Result           *PortalResult
}

// Expired 会话是否已过期或已完成。
func (s *PortalSession) Expired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage == portalDone || time.Now().After(s.ExpiresAt)
}

// Stage 当前阶段的对外名称：kiro（等待第 1 段回调）/ microsoft（等待第 2 段回调）/ done。
func (s *PortalSession) Stage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.stage {
	case portalWaitingProvider:
		return "microsoft"
	case portalDone:
		return "done"
	}
	return "kiro"
}

// Retire 清除会话里的全部敏感材料（完成 / 取消 / 过期时调用，幂等）。
func (s *PortalSession) Retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireLocked()
}

func (s *PortalSession) retireLocked() {
	s.stage = portalDone
	s.state, s.verifier, s.leg = "", "", nil
}

// StartPortalLogin 创建会话并返回第 1 段的登录 URL。
func (c *Client) StartPortalLogin() (*PortalSession, string) {
	s := &PortalSession{
		ID: newUUID(), ExpiresAt: time.Now().Add(portalSessionTTL), stage: portalWaitingPortal,
		state: randomURLSafe(32), verifier: randomURLSafe(64),
	}
	q := url.Values{}
	q.Set("state", s.state)
	q.Set("code_challenge", pkceChallenge(s.verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("redirect_uri", portalRedirectBase)
	q.Set("redirect_from", portalRedirectFrom)
	return s, portalSignInURL + "?" + q.Encode()
}

// ContinuePortalLogin 处理用户粘回的一个回调 URL。
func (c *Client) ContinuePortalLogin(ctx context.Context, s *PortalSession, callbackURL string) (PortalProgress, *Error) {
	if s == nil {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "登录会话不存在或已过期，请重新发起"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage == portalDone || time.Now().After(s.ExpiresAt) {
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "登录会话不存在或已过期，请重新发起"}
	}
	switch s.stage {
	case portalWaitingPortal:
		return c.portalFirstLeg(ctx, s, callbackURL)
	case portalWaitingProvider:
		return c.portalSecondLeg(ctx, s, callbackURL)
	}
	return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "登录会话状态无效"}
}

// parseLoopbackCallback 回调必须是 http://localhost:3128/...（或 127.0.0.1），不带 userinfo / fragment。
// wantPath 非空时路径必须完全一致。
func parseLoopbackCallback(raw, wantPath string) (url.Values, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", fmt.Errorf("请粘贴浏览器地址栏里的完整回调地址")
	}
	if len(raw) > portalMaxCallback {
		return nil, "", fmt.Errorf("回调地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return nil, "", fmt.Errorf("回调地址无效")
	}
	host := strings.ToLower(u.Hostname())
	if !strings.EqualFold(u.Scheme, "http") || (host != "localhost" && host != "127.0.0.1") ||
		u.Port() != "3128" || u.User != nil || u.Fragment != "" {
		return nil, "", fmt.Errorf("回调地址必须以 %s 开头（浏览器显示「无法访问 localhost」的那个页面的地址）", portalRedirectBase)
	}
	if wantPath != "" && u.Path != wantPath {
		return nil, "", fmt.Errorf("回调地址的路径应为 %s", wantPath)
	}
	return u.Query(), u.Path, nil
}

// single 取查询参数的唯一值；重复出现视为非法（避免参数污染）。
func single(q url.Values, key string) (string, error) {
	vs, ok := q[key]
	if !ok {
		return "", nil
	}
	if len(vs) != 1 {
		return "", fmt.Errorf("回调地址里的 %s 出现了多次", key)
	}
	return strings.TrimSpace(vs[0]), nil
}

func requiredSingle(q url.Values, key string) (string, error) {
	v, err := single(q, key)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("回调地址缺少 %s", key)
	}
	return v, nil
}

func badRequest(err error) *Error { return &Error{Kind: KindBadRequest, Msg: err.Error()} }

func (c *Client) portalFirstLeg(ctx context.Context, s *PortalSession, raw string) (PortalProgress, *Error) {
	q, path, err := parseLoopbackCallback(raw, "")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	if path == portalOAuthPath {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "这是微软登录的回调地址，但当前还在第一步：请粘贴 Kiro 登录页跳转到 localhost:3128 的地址"}
	}
	if code, _ := single(q, "error"); code != "" {
		desc, _ := single(q, "error_description")
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "Kiro 登录失败：" + code + descSuffix(desc)}
	}
	state, err := single(q, "state")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	loginOption, err := single(q, "login_option")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	code, err := single(q, "code")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}

	switch {
	case strings.EqualFold(loginOption, AuthMethodExternalIDP):
		// 描述符里的 state 部分门户版本不带；带了就必须一致。
		if state != "" && !constEq(state, s.state) {
			return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "回调地址的 state 与本次登录不匹配（是不是粘了上一次登录的地址？）"}
		}
		return c.portalMicrosoftDescriptor(ctx, s, q)
	case loginOption != "":
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: fmt.Sprintf("暂不支持这种登录方式（login_option=%s）。AWS IAM Identity Center 请改用「IAM Identity Center」设备授权登录", loginOption)}
	case code != "":
		if state == "" || !constEq(state, s.state) {
			return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "回调地址的 state 与本次登录不匹配（是不是粘了上一次登录的地址？）"}
		}
		return c.portalSocialExchange(ctx, s, code)
	}
	return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "回调地址里既没有授权码（code）也没有企业身份源信息（login_option），请确认粘贴的是登录完成后跳转到 localhost:3128 的完整地址"}
}

func descSuffix(desc string) string {
	if d := idpDesc(desc); d != "" {
		return "：" + d
	}
	return ""
}

// portalSocialExchange Google / GitHub：用 PKCE verifier 把授权码换成 Kiro token。
func (c *Client) portalSocialExchange(ctx context.Context, s *PortalSession, code string) (PortalProgress, *Error) {
	if len(code) > 8192 {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "授权码过长"}
	}
	status, raw, err := c.postJSON(ctx, socialTokenURL, map[string]string{
		"code": code, "code_verifier": s.verifier, "redirect_uri": portalRedirectBase,
	})
	if err != nil {
		// 授权码一次性：传输失败时保留会话，用户可以重试同一个回调。
		return PortalProgress{}, transportError(err)
	}
	if status != http.StatusOK {
		s.retireLocked() // 授权码已作废，必须重新登录
		kind := classifyRefresh(status, string(raw))
		if kind == KindServer && status < 500 {
			kind = KindBadRequest
		}
		return PortalProgress{}, &Error{Kind: kind, Status: status, Msg: "社交登录换取 token 失败（授权码可能已过期或被使用过，请重新登录）", Body: truncate(string(raw), 500)}
	}
	var r struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if json.Unmarshal(raw, &r) != nil || strings.TrimSpace(r.AccessToken) == "" || strings.TrimSpace(r.RefreshToken) == "" {
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindServer, Msg: "社交登录的 token 响应缺少 accessToken / refreshToken", Body: truncate(string(raw), 300)}
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 3600
	}
	res := &PortalResult{Kind: "social", Tokens: TokenSet{
		AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: time.Now().Unix() + r.ExpiresIn,
		ProfileArn: strings.TrimSpace(r.ProfileArn),
	}}
	s.retireLocked()
	return PortalProgress{Result: res}, nil
}

// portalMicrosoftDescriptor 企业 SSO 第 1 段：校验描述符 → OIDC 发现 → 生成微软授权 URL。
func (c *Client) portalMicrosoftDescriptor(ctx context.Context, s *PortalSession, q url.Values) (PortalProgress, *Error) {
	issuerURL, err := requiredSingle(q, "issuer_url")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	clientID, err := requiredSingle(q, "client_id")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	scopes, err := requiredSingle(q, "scopes")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	hint, err := single(q, "login_hint")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	if len(hint) > 320 {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "login_hint 过长"}
	}
	if scopes, err = NormalizeExternalIdpScopes(scopes, clientID); err != nil {
		return PortalProgress{}, badRequest(err)
	}
	authorize, tokenEndpoint, kerr := c.discoverMicrosoftOIDC(ctx, issuerURL)
	if kerr != nil {
		return PortalProgress{}, kerr
	}
	leg := &portalProviderLeg{
		State: randomURLSafe(32), Verifier: randomURLSafe(64),
		IssuerURL: strings.TrimRight(strings.TrimSpace(issuerURL), "/"), TokenEndpoint: tokenEndpoint,
		ClientID: clientID, Scopes: scopes,
	}
	s.leg, s.stage = leg, portalWaitingProvider

	p := url.Values{}
	p.Set("client_id", clientID)
	p.Set("response_type", "code")
	p.Set("redirect_uri", portalRedirectBase+portalOAuthPath)
	p.Set("scope", scopes)
	p.Set("code_challenge", pkceChallenge(leg.Verifier))
	p.Set("code_challenge_method", "S256")
	p.Set("response_mode", "query")
	p.Set("state", leg.State)
	if hint != "" {
		p.Set("login_hint", hint)
	}
	return PortalProgress{AuthorizationURL: authorize + "?" + p.Encode()}, nil
}

// portalSecondLeg 企业 SSO 第 2 段：微软回调 → 换 token。
func (c *Client) portalSecondLeg(ctx context.Context, s *PortalSession, raw string) (PortalProgress, *Error) {
	q, _, err := parseLoopbackCallback(raw, portalOAuthPath)
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	leg := s.leg
	if leg == nil {
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "登录会话状态丢失，请重新发起"}
	}
	state, err := requiredSingle(q, "state")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	if !constEq(state, leg.State) {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "回调地址的 state 与本次登录不匹配（是不是粘了上一次登录的地址？）"}
	}
	if code, _ := single(q, "error"); code != "" {
		desc, _ := single(q, "error_description")
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "Microsoft 授权失败：" + code + descSuffix(desc)}
	}
	code, err := requiredSingle(q, "code")
	if err != nil {
		return PortalProgress{}, badRequest(err)
	}
	if len(code) > 8192 {
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "授权码过长"}
	}
	form := url.Values{}
	form.Set("client_id", leg.ClientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", portalRedirectBase+portalOAuthPath)
	form.Set("code_verifier", leg.Verifier)
	form.Set("scope", leg.Scopes)
	tok, kerr := c.postIdpToken(ctx, leg.TokenEndpoint, leg.IssuerURL, form)
	if kerr != nil {
		if kerr.Kind != KindTransport {
			s.retireLocked() // 授权码已作废
		}
		return PortalProgress{}, kerr
	}
	if strings.TrimSpace(tok.RefreshToken) == "" {
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindServer, Msg: "Microsoft 没有返回 refresh token（scope 里需要 offline_access）"}
	}
	meta := jwtMeta(tok.AccessToken)
	if meta.Issuer != "" && !sameURL(meta.Issuer, leg.IssuerURL) {
		s.retireLocked()
		return PortalProgress{}, &Error{Kind: KindBadRequest, Msg: "Microsoft access token 的签发者与登录签发者不一致"}
	}
	email, userID := externalIdpIdentity(tok.AccessToken)
	res := &PortalResult{
		Kind:     AuthMethodExternalIDP,
		Tokens:   TokenSet{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: time.Now().Unix() + tok.ExpiresIn},
		ClientID: leg.ClientID, TokenEndpoint: leg.TokenEndpoint, IssuerURL: leg.IssuerURL, Scopes: leg.Scopes,
		Email: email, UserID: userID,
	}
	s.retireLocked()
	return PortalProgress{Result: res}, nil
}
