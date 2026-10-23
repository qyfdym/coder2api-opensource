package kiro

// Microsoft 企业 SSO（Entra ID / Azure AD，authMethod = "external_idp"）。
//
// 这类账号的 access / refresh token 由微软租户签发，刷新走租户的 OAuth2 端点（表单提交、公共客户端、
// 不带 client_secret），推理请求带 TokenType: EXTERNAL_IDP。移植自 Kiro-Go auth/microsoft_sso.go，
// 安全约束原样保留：
//   - refresh token 只会发给白名单里的微软登录域名（login.microsoftonline.com / .us / partner .cn），
//     且端点路径必须与签发者租户一致；
//   - 所有 IdP 请求不跟随重定向、响应体限长；
//   - 持久化的 issuer / tokenEndpoint / scopes 在每次刷新前重新校验。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// AuthMethodExternalIDP / ProviderAzureAD 账号文件里的取值（与 Kiro-Go 一致）。
	AuthMethodExternalIDP = "external_idp"
	ProviderAzureAD       = "AzureAD"

	maxIdpResponse = 1 << 20
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// allowedIdpHosts 允许发送 refresh token 的微软登录域名。
var allowedIdpHosts = map[string]bool{
	"login.microsoftonline.com":        true,
	"login.microsoftonline.us":         true,
	"login.partner.microsoftonline.cn": true,
}

// ValidateExternalIdpEndpoint 校验任何可能携带微软 token 的出站 URL：只允许白名单域名的
// https 默认端口；拒绝 userinfo、fragment、IP 字面量。
func ValidateExternalIdpEndpoint(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return fmt.Errorf("外部身份源地址无效")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("外部身份源地址必须是 https")
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("外部身份源地址含有不允许的组成部分")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("外部身份源地址必须使用默认 https 端口")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("外部身份源主机名无效")
	}
	if !allowedIdpHosts[host] {
		return fmt.Errorf("外部身份源主机 %q 不在白名单内（仅支持 Microsoft Entra 登录域名）", host)
	}
	return nil
}

// parseMicrosoftIssuer 签发者必须是 https://<白名单主机>/<租户 UUID>/v2.0。
func parseMicrosoftIssuer(raw string) (*url.URL, string, error) {
	if err := ValidateExternalIdpEndpoint(raw); err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if u.RawQuery != "" {
		return nil, "", fmt.Errorf("Microsoft 签发者地址不能带查询参数")
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segs) != 2 || !strings.EqualFold(segs[1], "v2.0") {
		return nil, "", fmt.Errorf("Microsoft 签发者地址必须形如 /<租户>/v2.0")
	}
	tenant, err := url.PathUnescape(segs[0])
	if err != nil || !uuidPattern.MatchString(tenant) {
		return nil, "", fmt.Errorf("Microsoft 租户必须是 UUID")
	}
	return u, tenant, nil
}

// validateMicrosoftEndpoint 端点与签发者同主机、同租户，路径固定为 /<租户><suffix>。
func validateMicrosoftEndpoint(raw string, issuer *url.URL, tenant, suffix string) error {
	if err := ValidateExternalIdpEndpoint(raw); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	if !strings.EqualFold(u.Hostname(), issuer.Hostname()) {
		return fmt.Errorf("端点主机与签发者主机不一致")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("端点不能带查询参数")
	}
	if !strings.EqualFold(strings.TrimRight(u.EscapedPath(), "/"), "/"+tenant+suffix) {
		return fmt.Errorf("端点与签发者租户不一致")
	}
	return nil
}

var (
	standardScopes = map[string]bool{"openid": true, "profile": true, "email": true, "offline_access": true}
	kiroScopeNames = map[string]bool{
		"codewhisperer:completions": true, "codewhisperer:analysis": true, "codewhisperer:conversations": true,
		"codewhisperer:transformations": true, "codewhisperer:taskassist": true,
	}
)

// NormalizeExternalIdpScopes 校验并规整 scope：只允许标准 OIDC scope 与本应用的 Kiro 资源 scope，
// 必须含 offline_access（否则没有 refresh token）和至少一个 Kiro 资源 scope。
func NormalizeExternalIdpScopes(raw, clientID string) (string, error) {
	clientID = strings.TrimSpace(clientID)
	if !uuidPattern.MatchString(clientID) {
		return "", fmt.Errorf("Microsoft client_id 不是合法的 UUID")
	}
	if len(raw) > 4096 {
		return "", fmt.Errorf("Microsoft scope 过长")
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 || len(fields) > 16 {
		return "", fmt.Errorf("Microsoft scope 无效")
	}
	prefix := "api://" + clientID + "/"
	seen := map[string]bool{}
	var out []string
	hasOffline, hasKiro := false, false
	for _, sc := range fields {
		if seen[sc] {
			continue
		}
		switch {
		case standardScopes[sc]:
			hasOffline = hasOffline || sc == "offline_access"
		case strings.HasPrefix(sc, prefix) && kiroScopeNames[strings.TrimPrefix(sc, prefix)]:
			hasKiro = true
		default:
			return "", fmt.Errorf("Microsoft scope %q 不是 Kiro 预期的 scope", sc)
		}
		seen[sc] = true
		out = append(out, sc)
	}
	if !hasOffline {
		return "", fmt.Errorf("Microsoft scope 必须包含 offline_access")
	}
	if !hasKiro {
		return "", fmt.Errorf("Microsoft scope 不含 Kiro 资源 scope")
	}
	return strings.Join(out, " "), nil
}

// ValidateExternalIdpConfig 刷新（或入库）前校验：scope 合法，且 tokenEndpoint 与 issuer 同租户。
func ValidateExternalIdpConfig(clientID, tokenEndpoint, issuerURL, scopes string) error {
	if _, err := NormalizeExternalIdpScopes(scopes, clientID); err != nil {
		return err
	}
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return fmt.Errorf("外部身份源签发者被拒绝：%w", err)
	}
	if err := validateMicrosoftEndpoint(tokenEndpoint, issuer, tenant, "/oauth2/v2.0/token"); err != nil {
		return fmt.Errorf("外部身份源 token 端点被拒绝：%w", err)
	}
	return nil
}

// ExternalIdpConfigFromIssuer 由签发者与 clientId 推出租户 token 端点与默认 scope。
func ExternalIdpConfigFromIssuer(issuerURL, clientID string) (tokenEndpoint, normalizedIssuer, scopes string) {
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return "", "", ""
	}
	clientID = strings.TrimSpace(clientID)
	if !uuidPattern.MatchString(clientID) {
		return "", "", ""
	}
	tokenEndpoint = issuer.Scheme + "://" + issuer.Host + "/" + tenant + "/oauth2/v2.0/token"
	scopes = strings.Join([]string{
		"api://" + clientID + "/codewhisperer:conversations",
		"api://" + clientID + "/codewhisperer:completions",
		"offline_access",
	}, " ")
	return tokenEndpoint, strings.TrimRight(issuer.String(), "/"), scopes
}

// ExternalIdpConfigFromTokenEndpoint 导出文件只有租户 token 端点、没有 issuerUrl 时反推。
func ExternalIdpConfigFromTokenEndpoint(tokenEndpoint, clientID string) (normalizedEndpoint, issuerURL, scopes string) {
	if ValidateExternalIdpEndpoint(tokenEndpoint) != nil {
		return "", "", ""
	}
	u, err := url.Parse(strings.TrimSpace(tokenEndpoint))
	if err != nil || u.RawQuery != "" {
		return "", "", ""
	}
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segs) != 4 || !strings.EqualFold(segs[1], "oauth2") || !strings.EqualFold(segs[2], "v2.0") || !strings.EqualFold(segs[3], "token") {
		return "", "", ""
	}
	tenant, err := url.PathUnescape(segs[0])
	if err != nil || !uuidPattern.MatchString(tenant) {
		return "", "", ""
	}
	return ExternalIdpConfigFromIssuer(u.Scheme+"://"+u.Host+"/"+tenant+"/v2.0", clientID)
}

// issuerFromKAMUserID Kiro Account Manager 导出的 userId 形如 https://login.microsoftonline.com/<租户>/v2.0.<oid>。
func issuerFromKAMUserID(userID string) string {
	u, err := url.Parse(strings.TrimSpace(userID))
	if err != nil || u.Host == "" {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 {
		return ""
	}
	v := strings.ToLower(segs[1])
	if v != "v2.0" && !strings.HasPrefix(v, "v2.0.") {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/" + segs[0] + "/v2.0"
}

// DeriveExternalIdpConfig 导入时补全缺失的端点：先看 KAM userId，再看 access token 的 iss 声明。
func DeriveExternalIdpConfig(userID, clientID, accessToken string) (tokenEndpoint, issuerURL, scopes string) {
	issuerURL = issuerFromKAMUserID(userID)
	if issuerURL == "" {
		issuerURL = jwtMeta(accessToken).Issuer
	}
	tokenEndpoint, issuerURL, scopes = ExternalIdpConfigFromIssuer(issuerURL, clientID)
	return tokenEndpoint, issuerURL, scopes
}

// tokenMeta access token（JWT）里仅用于展示的身份声明。授权从不依赖这些未验签的声明。
type tokenMeta struct {
	Issuer   string
	Email    string
	ObjectID string
	Expiry   int64
}

// ExternalIdpIdentity 由企业 SSO 的 access token 取（邮箱，稳定用户标识）。仅作展示 / 账号去重：
// token 没有验签，授权从不依赖这些声明。
func ExternalIdpIdentity(accessToken string) (email, userID string) {
	return externalIdpIdentity(accessToken)
}

func jwtMeta(accessToken string) tokenMeta {
	if accessToken == "" || len(accessToken) > 256<<10 {
		return tokenMeta{}
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return tokenMeta{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenMeta{}
	}
	var c struct {
		Iss               string `json:"iss"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
		UniqueName        string `json:"unique_name"`
		OID               string `json:"oid"`
		Exp               int64  `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return tokenMeta{}
	}
	m := tokenMeta{Issuer: strings.TrimRight(strings.TrimSpace(c.Iss), "/"), ObjectID: strings.TrimSpace(c.OID), Expiry: c.Exp}
	m.Email = firstNonEmptyStr(strings.TrimSpace(c.Email), strings.TrimSpace(c.PreferredUsername), strings.TrimSpace(c.UPN), strings.TrimSpace(c.UniqueName))
	return m
}

// externalIdpIdentity 由 access token 取（邮箱，稳定用户标识 = <iss>.<oid>），仅作展示 / 账号去重。
func externalIdpIdentity(accessToken string) (email, userID string) {
	m := jwtMeta(accessToken)
	if m.Issuer != "" && m.ObjectID != "" {
		userID = m.Issuer + "." + m.ObjectID
	}
	return m.Email, userID
}

// ---------------------------------------------------------------------------
// 出站请求
// ---------------------------------------------------------------------------

// noRedirect IdP 调用的 HTTP 客户端：不跟随重定向（重定向可能把 token 带去别的主机）。
func (c *Client) noRedirect() *http.Client {
	cl := *c.auth
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cl
}

type idpToken struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func idpDesc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// postIdpToken 向租户 token 端点提交表单（authorization_code 或 refresh_token）。
func (c *Client) postIdpToken(ctx context.Context, tokenEndpoint, issuerURL string, form url.Values) (*idpToken, *Error) {
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return nil, &Error{Kind: KindSessionDead, Msg: "外部身份源 token 端点被拒绝：" + err.Error()}
	}
	if strings.TrimSpace(issuerURL) != "" {
		issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
		if err == nil {
			err = validateMicrosoftEndpoint(tokenEndpoint, issuer, tenant, "/oauth2/v2.0/token")
		}
		if err != nil {
			return nil, &Error{Kind: KindSessionDead, Msg: "外部身份源配置被拒绝：" + err.Error()}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &Error{Kind: KindBadRequest, Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.noRedirect().Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxIdpResponse))
	var tok idpToken
	_ = json.Unmarshal(raw, &tok)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &Error{Kind: classifyRefresh(resp.StatusCode, string(raw)), Status: resp.StatusCode, Code: tok.Error,
			Msg: "Microsoft 令牌请求失败：" + friendlyMessage(classifyRefresh(resp.StatusCode, string(raw)))}
		if d := idpDesc(tok.ErrorDescription); d != "" {
			e.Msg += "（" + tok.Error + "：" + d + "）"
		}
		e.Body = truncate(string(raw), 500)
		return nil, e
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, &Error{Kind: KindServer, Status: resp.StatusCode, Msg: "Microsoft 令牌响应缺少 access_token"}
	}
	if tok.ExpiresIn <= 0 {
		return nil, &Error{Kind: KindServer, Status: resp.StatusCode, Msg: "Microsoft 令牌响应的 expires_in 无效"}
	}
	return &tok, nil
}

// refreshExternalIdp 刷新 external_idp 账号：表单、公共客户端（无 secret / redirect_uri）。
// 微软可能轮换 refresh token，响应里没有时沿用旧的。
func (c *Client) refreshExternalIdp(ctx context.Context, cred Cred) (TokenSet, *Error) {
	if strings.TrimSpace(cred.ClientID) == "" || strings.TrimSpace(cred.TokenEndpoint) == "" {
		return TokenSet{}, &Error{Kind: KindSessionDead, Msg: "企业 SSO 账号缺少 clientId / tokenEndpoint，需要重新登录"}
	}
	scopes, err := NormalizeExternalIdpScopes(cred.Scopes, cred.ClientID)
	if err != nil {
		return TokenSet{}, &Error{Kind: KindSessionDead, Msg: "企业 SSO 账号的 scope 无效：" + err.Error()}
	}
	if err := ValidateExternalIdpConfig(cred.ClientID, cred.TokenEndpoint, cred.IssuerURL, scopes); err != nil {
		return TokenSet{}, &Error{Kind: KindSessionDead, Msg: "企业 SSO 账号配置被拒绝：" + err.Error()}
	}
	form := url.Values{}
	form.Set("client_id", cred.ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	form.Set("scope", scopes)
	tok, kerr := c.postIdpToken(ctx, cred.TokenEndpoint, cred.IssuerURL, form)
	if kerr != nil {
		return TokenSet{}, kerr
	}
	ts := TokenSet{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: time.Now().Unix() + tok.ExpiresIn}
	if ts.RefreshToken == "" {
		ts.RefreshToken = cred.RefreshToken
	}
	return ts, nil
}

// discoverMicrosoftOIDC 读签发者的 OIDC 发现文档，并校验授权 / token 端点都属于同一租户。
func (c *Client) discoverMicrosoftOIDC(ctx context.Context, issuerURL string) (authorize, token string, kerr *Error) {
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return "", "", &Error{Kind: KindBadRequest, Msg: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer.String(), "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", "", &Error{Kind: KindBadRequest, Msg: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.noRedirect().Do(req)
	if err != nil {
		return "", "", transportError(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxIdpResponse))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", &Error{Kind: KindServer, Status: resp.StatusCode, Msg: fmt.Sprintf("Microsoft OIDC 发现失败（HTTP %d）", resp.StatusCode)}
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", "", &Error{Kind: KindServer, Msg: "Microsoft OIDC 发现文档解析失败"}
	}
	if !sameURL(doc.Issuer, issuer.String()) {
		return "", "", &Error{Kind: KindBadRequest, Msg: "Microsoft OIDC 发现文档的 issuer 与请求不一致"}
	}
	if err := validateMicrosoftEndpoint(doc.AuthorizationEndpoint, issuer, tenant, "/oauth2/v2.0/authorize"); err != nil {
		return "", "", &Error{Kind: KindBadRequest, Msg: "Microsoft 授权端点被拒绝：" + err.Error()}
	}
	if err := validateMicrosoftEndpoint(doc.TokenEndpoint, issuer, tenant, "/oauth2/v2.0/token"); err != nil {
		return "", "", &Error{Kind: KindBadRequest, Msg: "Microsoft token 端点被拒绝：" + err.Error()}
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
}

func constEq(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomURLSafe(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
