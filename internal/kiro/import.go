package kiro

// 凭证导入文本解析（对齐 Kiro-Go 面板的「凭证 JSON」导入口径）。
//
// 接受的形态（可混合，一次导入多个账号）：
//   - JSON 对象 / 数组：Kiro-Go 账号对象、Kiro Account Manager 导出（{credentials:{…}}）、
//     {accounts:[…]}、Kiro IDE 的 ~/.aws/sso/cache/kiro-auth-token.json；
//     IdC 账号的 token 文件不含 clientId / clientSecret，它们在同目录 <clientIdHash>.json 里——
//     两个文件的内容放进同一个数组即可自动配对；
//   - 行格式：邮箱----密码----refreshToken----clientId----clientSecret（分隔符也可以是 Tab /
//     空白；密码列只为兼容常见的导出格式，不会被使用或保存）；
//   - 单独一行 refresh token（社交登录）或 ksk_ 开头的 Kiro API Key（可写 ksk_xxx|区域）。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ImportItem 一个待导入的账号凭证（已归一）。
type ImportItem struct {
	RefreshToken string
	AccessToken  string
	ExpiresAt    int64
	ClientID     string
	ClientSecret string
	AuthMethod   string // social / idc / external_idp / api_key
	Provider     string
	Region       string
	StartURL     string
	ProfileArn   string
	APIKey       string
	Email        string
	Nickname     string
	// 仅 external_idp（Microsoft 企业 SSO）：租户 token 端点 / 签发者 / scope，以及 JWT 里的用户标识。
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	UserID        string
}

// ParseImport 解析导入文本。返回可导入的条目，以及被跳过的条目说明（格式不全 / 不支持）。
func ParseImport(text string) ([]ImportItem, []string) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return nil, []string{"内容为空"}
	}
	if text[0] == '{' || text[0] == '[' {
		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			return nil, []string{"JSON 格式错误：" + err.Error()}
		}
		return normalizeRecords(flattenRecords(v))
	}
	var items []ImportItem
	var skipped []string
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var parts []string
		switch {
		case strings.Contains(line, "----"):
			parts = strings.Split(line, "----")
		case strings.Contains(line, "\t"):
			parts = strings.Split(line, "\t")
		default:
			parts = strings.Fields(line)
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		switch {
		case len(parts) >= 5 && parts[2] != "":
			// 邮箱----密码----refreshToken----clientId----clientSecret：密码列丢弃。
			it := ImportItem{
				Email: parts[0], RefreshToken: parts[2], ClientID: parts[3], ClientSecret: parts[4],
				AuthMethod: "idc", Provider: "BuilderId", Region: defaultRegion,
			}
			if it.ClientID == "" || it.ClientSecret == "" {
				it.AuthMethod, it.Provider = "social", "Social"
			}
			items = append(items, it)
		case len(parts) == 1 && looksLikeAPIKey(parts[0]):
			it, err := apiKeyItem(parts[0], "")
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("第 %d 行：%v", n+1, err))
				continue
			}
			items = append(items, it)
		case len(parts) == 1 && len(parts[0]) >= 32:
			// 单独的 refresh token：社交登录（app.kiro.dev 网页登录后取到的就是这种）。
			items = append(items, ImportItem{RefreshToken: parts[0], AuthMethod: "social", Provider: "Social", Region: defaultRegion})
		default:
			skipped = append(skipped, fmt.Sprintf("第 %d 行：格式不完整（需要 邮箱----密码----refreshToken----clientId----clientSecret，或单独的 refresh token / API Key）", n+1))
		}
	}
	return items, skipped
}

func looksLikeAPIKey(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '|'); i > 0 {
		s = s[:i]
	}
	return strings.HasPrefix(s, "ksk_")
}

// apiKeyItem 解析 "ksk_xxx" / "ksk_xxx|eu-central-1"。
func apiKeyItem(raw, region string) (ImportItem, error) {
	key := strings.TrimSpace(raw)
	if i := strings.IndexByte(key, '|'); i >= 0 {
		if strings.Count(key, "|") > 1 {
			return ImportItem{}, fmt.Errorf("API Key 里只能有一个 |")
		}
		r := strings.ToLower(strings.TrimSpace(key[i+1:]))
		key = strings.TrimSpace(key[:i])
		if r != "" {
			region = r
		}
	}
	if key == "" {
		return ImportItem{}, fmt.Errorf("API Key 为空")
	}
	if region == "" {
		region = defaultRegion
	}
	if !regionPattern.MatchString(region) {
		return ImportItem{}, fmt.Errorf("区域 %q 不合法", region)
	}
	return ImportItem{APIKey: key, AccessToken: key, AuthMethod: "api_key", Provider: "APIKey", Region: region}, nil
}

// flattenRecords JSON 值 → 账号对象列表（展开 {accounts:[…]} 与数组）。
func flattenRecords(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			out = append(out, flattenRecords(x)...)
		}
	case map[string]any:
		if arr, ok := t["accounts"].([]any); ok {
			for _, x := range arr {
				out = append(out, flattenRecords(x)...)
			}
			return out
		}
		out = append(out, t)
	}
	return out
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

// recordValue Kiro Account Manager 导出把凭证放在 credentials 子对象里：子对象优先。
func recordValue(rec map[string]any, keys ...string) string {
	if cred, ok := rec["credentials"].(map[string]any); ok {
		if s := strField(cred, keys...); s != "" {
			return s
		}
	}
	return strField(rec, keys...)
}

func parseExpiresAt(rec map[string]any) int64 {
	raw := rec["expiresAt"]
	if cred, ok := rec["credentials"].(map[string]any); ok && cred["expiresAt"] != nil {
		raw = cred["expiresAt"]
	}
	switch v := raw.(type) {
	case float64:
		n := int64(v)
		if n > 1e12 {
			n /= 1000
		}
		return n
	case string:
		s := strings.TrimSpace(v)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.Unix()
			}
		}
		// Kiro Account Manager 导出的是不带时区的本地时间（"2026/10/23 23:47:28"），按本机时区解释。
		for _, layout := range []string{"2006/01/02 15:04:05", "2006-01-02 15:04:05"} {
			if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
				return t.Unix()
			}
		}
	}
	return 0
}

var externalIdpWords = map[string]bool{
	"external_idp": true, "external-idp": true, "external": true, "microsoft": true, "m365": true,
	"office365": true, "azure": true, "azuread": true, "azure-ad": true, "azure_ad": true, "entra": true, "entra-id": true,
}

// externalIdpItem Microsoft 企业 SSO 记录 → 导入条目。缺失的 tokenEndpoint / issuerUrl / scopes
// 依次由 issuer、tokenEndpoint、Kiro Account Manager 的 userId、access token 的 iss 声明补全；
// 补全后必须通过白名单与租户一致性校验，clientSecret 一律丢弃（公共客户端不需要）。
func externalIdpItem(r map[string]any) (ImportItem, string) {
	rt := recordValue(r, "refreshToken", "refresh_token")
	if rt == "" {
		return ImportItem{}, "缺少 refreshToken"
	}
	clientID := recordValue(r, "clientId", "client_id")
	if !uuidPattern.MatchString(clientID) {
		return ImportItem{}, "企业 SSO 账号需要 clientId（Microsoft 应用的 UUID）"
	}
	at := recordValue(r, "accessToken", "access_token")
	tokenEndpoint := recordValue(r, "tokenEndpoint", "token_endpoint")
	issuer := strings.TrimRight(recordValue(r, "issuerUrl", "issuer_url"), "/")
	scopes := recordValue(r, "scopes", "scope")
	userID := recordValue(r, "userId")

	dEndpoint, dIssuer, dScopes := DeriveExternalIdpConfig(userID, clientID, at)
	if issuer == "" && tokenEndpoint != "" {
		if ep, iss, sc := ExternalIdpConfigFromTokenEndpoint(tokenEndpoint, clientID); ep != "" {
			tokenEndpoint, issuer = ep, iss
			if scopes == "" {
				scopes = sc
			}
		}
	}
	if issuer == "" {
		issuer = dIssuer
	}
	if issuer != "" {
		ep, iss, sc := ExternalIdpConfigFromIssuer(issuer, clientID)
		if tokenEndpoint == "" {
			tokenEndpoint = ep
		}
		if iss != "" {
			issuer = iss
		}
		if scopes == "" {
			scopes = sc
		}
	}
	if tokenEndpoint == "" {
		tokenEndpoint = dEndpoint
	}
	if scopes == "" {
		scopes = dScopes
	}
	if issuer == "" || tokenEndpoint == "" {
		return ImportItem{}, "无法确定企业 SSO 的租户（需要 issuerUrl 或 tokenEndpoint，或带 iss 声明的 accessToken）"
	}
	norm, err := NormalizeExternalIdpScopes(scopes, clientID)
	if err != nil {
		return ImportItem{}, err.Error()
	}
	if err := ValidateExternalIdpConfig(clientID, tokenEndpoint, issuer, norm); err != nil {
		return ImportItem{}, err.Error()
	}
	it := ImportItem{
		RefreshToken: rt, AccessToken: at, ExpiresAt: parseExpiresAt(r), ClientID: clientID,
		AuthMethod: AuthMethodExternalIDP, Provider: ProviderAzureAD, Region: defaultRegion,
		TokenEndpoint: tokenEndpoint, IssuerURL: issuer, Scopes: norm, UserID: userID,
		Email: recordValue(r, "email"), Nickname: recordValue(r, "nickname", "name"),
	}
	if p := recordValue(r, "profileArn", "profile_arn"); p != "" {
		arn, _, ok := ParseProfileArn(p)
		if !ok {
			return ImportItem{}, "profileArn 格式不合法"
		}
		it.ProfileArn = arn
	}
	return it, ""
}

// normalizeRecords 把 JSON 账号对象归一成 ImportItem，并把 Kiro IDE 的 token 文件与对应的
// OIDC 客户端文件（只有 clientId / clientSecret）配对合并。
func normalizeRecords(recs []map[string]any) ([]ImportItem, []string) {
	var clients []map[string]any
	var accounts []map[string]any
	for _, r := range recs {
		hasCred := recordValue(r, "refreshToken", "refresh_token", "kiroApiKey", "accessToken") != ""
		if !hasCred && strField(r, "clientId") != "" && strField(r, "clientSecret") != "" {
			clients = append(clients, r)
			continue
		}
		accounts = append(accounts, r)
	}
	var items []ImportItem
	var skipped []string
	for i, r := range accounts {
		label := fmt.Sprintf("第 %d 个账号", i+1)
		method := strings.ToLower(recordValue(r, "authMethod", "auth_method"))
		provider := recordValue(r, "provider", "idp")
		// 显式的 AWS / 社交方式优先于记录里顺带出现的 tokenEndpoint / issuerUrl 字段，
		// 免得混合模板把一条 IdC 账号误判成企业 SSO。
		explicitAWS := method == "idc" || method == "builderid" || method == "enterprise" || method == "iam_identity_center" ||
			method == "social" || method == "google" || method == "github" || method == "api_key" || method == "apikey"
		implicitIdp := method == "" && provider == "" && recordValue(r, "tokenEndpoint", "issuerUrl", "token_endpoint", "issuer_url") != ""
		if !explicitAWS && (externalIdpWords[method] || externalIdpWords[strings.ToLower(provider)] || implicitIdp) {
			it, why := externalIdpItem(r)
			if why != "" {
				skipped = append(skipped, label+"："+why)
				continue
			}
			items = append(items, it)
			continue
		}
		region := strings.ToLower(recordValue(r, "region"))
		apiKey := recordValue(r, "kiroApiKey", "apiKey")
		rt := recordValue(r, "refreshToken", "refresh_token")
		at := recordValue(r, "accessToken", "access_token")
		if apiKey == "" && rt == "" && (method == "api_key" || method == "apikey" || looksLikeAPIKey(at)) {
			apiKey = at
		}
		if apiKey != "" {
			it, err := apiKeyItem(apiKey, region)
			if err != nil {
				skipped = append(skipped, label+"："+err.Error())
				continue
			}
			it.Email = recordValue(r, "email")
			it.Nickname = recordValue(r, "nickname", "name")
			items = append(items, it)
			continue
		}
		if rt == "" {
			skipped = append(skipped, label+"：缺少 refreshToken")
			continue
		}
		it := ImportItem{
			RefreshToken: rt, AccessToken: at, ExpiresAt: parseExpiresAt(r),
			ClientID: recordValue(r, "clientId", "client_id"), ClientSecret: recordValue(r, "clientSecret", "client_secret"),
			Provider: provider, Region: region, StartURL: recordValue(r, "startUrl", "start_url"),
			ProfileArn: recordValue(r, "profileArn", "profile_arn"),
			Email:      recordValue(r, "email"), Nickname: recordValue(r, "nickname", "name"),
		}
		// Kiro IDE 的 IdC token 文件：客户端凭证在配对的 <clientIdHash>.json 里。
		if it.ClientID == "" && len(clients) > 0 && (method == "idc" || strField(r, "clientIdHash") != "") {
			cl := clients[0]
			clients = clients[1:]
			it.ClientID, it.ClientSecret = strField(cl, "clientId"), strField(cl, "clientSecret")
		}
		switch {
		case method == "idc" || method == "builderid" || method == "enterprise" || method == "iam_identity_center":
			it.AuthMethod = "idc"
		case method == "social" || method == "google" || method == "github":
			it.AuthMethod = "social"
		case it.ClientID != "" && it.ClientSecret != "":
			it.AuthMethod = "idc"
		default:
			it.AuthMethod = "social"
		}
		if it.AuthMethod == "idc" && (it.ClientID == "" || it.ClientSecret == "") {
			skipped = append(skipped, label+"：IdC / Builder ID 账号缺少 clientId / clientSecret（Kiro IDE 的在 ~/.aws/sso/cache/<clientIdHash>.json 里，一起粘贴即可）")
			continue
		}
		if it.Provider == "" {
			if it.AuthMethod == "idc" {
				it.Provider = "BuilderId"
				if it.StartURL != "" && !strings.EqualFold(strings.TrimRight(it.StartURL, "/"), BuilderIDStartURL) {
					it.Provider = "Enterprise"
				}
			} else {
				it.Provider = "Social"
			}
		}
		if it.Region == "" || !regionPattern.MatchString(it.Region) {
			it.Region = defaultRegion
		}
		if it.ProfileArn != "" {
			if arn, _, ok := ParseProfileArn(it.ProfileArn); ok {
				it.ProfileArn = arn
			} else {
				it.ProfileArn = ""
			}
		}
		items = append(items, it)
	}
	return items, skipped
}
