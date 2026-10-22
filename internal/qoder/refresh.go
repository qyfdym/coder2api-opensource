package qoder

// access token 刷新（按 refresh token 前缀路由）。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）
// qoder_accounts.py：Account.refresh / Account._post_token / token_family。
//
// 两个凭证族：
//
//	设备族（OAuth 设备授权）：dt- access ≈30 天，drt- refresh ≈1 年，**每次刷新都会换新 drt-**；
//	job 族（PAT 交换）      ：jt- access 24 小时，jrt- refresh 48 小时，pt- PAT 长期有效。
//
// 路由：drt- → deviceToken/refresh（不回落 PAT：设备会话是主凭证，PAT 永不覆盖活跃的 OAuth
// 会话）；jrt- → jobToken/refresh，失败再用 PAT 走 jobToken/exchange 重新换一套。
//
// 落盘纪律（调用方负责）：刷新成功后**必须立刻**把新 AccessToken / RefreshToken / ExpiresAt
// 原子写回账号文件——drt- 是一次性的，旧值在服务端已经作废，丢了新值账号就只能重新登录。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	tokenKindDevice = "device"
	tokenKindJob    = "job"
)

// TokenFamily 凭证族：device（drt-，OAuth）/ job（jrt-，PAT 交换）/ pat（access 本身是 pt-）/ unknown。
func TokenFamily(cred Cred) string {
	rt := cred.RefreshToken
	switch {
	case strings.HasPrefix(rt, "drt-"):
		return "device"
	case strings.HasPrefix(rt, "jrt-"):
		return "job"
	case strings.HasPrefix(cred.AccessToken, "pt-"):
		return "pat"
	default:
		return "unknown"
	}
}

// Refresh 刷新 access token，返回新的 TokenSet（调用方写回并落盘）。
//
// 错误语义：
//   - KindSessionDead：上游吊销了会话（TOKEN_EXPIRE / 12153 / Offline user session not found），
//     刷新已无意义，调用方应停用账号并提示「需重新登录」（原实现置 enabled=False）；
//   - KindBadRequest + Code "NO_REFRESH_TOKEN"：既没有 refresh token 也没有 PAT（只能重新登录，
//     但现有 access token 可能仍然有效，是否停用由调用方按到期时间决定）；
//   - KindTransport / KindServer：暂时性失败，稍后再试即可。
//
// jrt- 与 PAT 都失败时的取舍：PAT 那一步是暂时性失败（网络 / 5xx / 429）→ 返回它（别因为一次
// 网络抖动就把可救的账号判死）；否则只要任何一步给出会话死亡就返回会话死亡；再否则返回最后一个错误。
func (c *Client) Refresh(ctx context.Context, cred Cred) (TokenSet, *Error) {
	rt := strings.TrimSpace(cred.RefreshToken)
	pat := strings.TrimSpace(cred.PAT)
	if strings.HasPrefix(rt, "drt-") {
		ts, err := c.postToken(ctx, cred, oaPathDeviceRefresh, map[string]string{"refresh_token": rt}, tokenKindDevice)
		if err == nil {
			c.Sessions.Invalidate(cred.UID)
		}
		return ts, err
	}
	var errs []*Error
	if rt != "" {
		ts, err := c.postToken(ctx, cred, oaPathJobRefresh, map[string]string{"refresh_token": rt}, tokenKindJob)
		if err == nil {
			c.Sessions.Invalidate(cred.UID)
			return ts, nil
		}
		errs = append(errs, err)
	}
	if pat != "" {
		ts, err := c.postToken(ctx, cred, oaPathJobExchange, map[string]string{"personal_token": pat}, tokenKindJob)
		if err == nil {
			c.Sessions.Invalidate(cred.UID)
			return ts, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return TokenSet{}, &Error{Kind: KindBadRequest, Code: "NO_REFRESH_TOKEN", Msg: "no refresh token; sign in again"}
	}
	return TokenSet{}, pickRefreshError(errs)
}

// pickRefreshError 多步刷新都失败时选出最该交给调用方的那个错误（规则见 Refresh 注释）。
func pickRefreshError(errs []*Error) *Error {
	last := errs[len(errs)-1]
	switch last.Kind {
	case KindTransport, KindServer, KindRateLimit:
		return last
	}
	for _, e := range errs {
		if e.Kind == KindSessionDead {
			return e
		}
	}
	return last
}

// postToken 一次 token 端点调用（原实现 _post_token：retries=3、timeout 30s、区域 UA）。
//
// 响应字段：
//
//	device：token（或 device_token）、refresh_token（缺失沿用旧值）、到期见 deviceExpiry；
//	job   ：token、refresh_token（缺失沿用旧值）、expires_in（毫秒）。
//
// job 族缺 expires_in 时与原实现不同：原实现沿用旧的 expiresAt（多半已过期，保活会每轮重刷），
// 这里按 jt- 的标称寿命 24 小时计。
func (c *Client) postToken(ctx context.Context, cred Cred, path string, payload map[string]string, kind string) (TokenSet, *Error) {
	realm := NormalizeRealm(cred.Realm)
	body, _ := json.Marshal(payload)
	resp, err := c.oaDo(ctx, oaRequest{method: http.MethodPost, url: oaURL(realm, path), body: body,
		header: oaTokenHeaders(realm, ""), attempts: 3, timeout: 30 * time.Second})
	if err != nil {
		if err.Kind != KindTransport {
			err.Msg = "refresh failed: " + err.Msg
		}
		return TokenSet{}, err
	}
	data, _ := oaObject(resp.body)
	now := time.Now()
	ts := TokenSet{RefreshToken: oaStr(data["refresh_token"])}
	if ts.RefreshToken == "" {
		ts.RefreshToken = cred.RefreshToken
	}
	if kind == tokenKindDevice {
		ts.AccessToken = oaStr(data["token"])
		if ts.AccessToken == "" {
			ts.AccessToken = oaStr(data["device_token"])
		}
		ts.ExpiresAt = deviceExpiry(data, now)
	} else {
		ts.AccessToken = oaStr(data["token"])
		if oaTruthy(data["expires_in"]) {
			ts.ExpiresAt = now.Unix() + oaInt(data["expires_in"])/1000
		} else {
			ts.ExpiresAt = now.Unix() + 24*3600
		}
	}
	if ts.AccessToken == "" {
		return TokenSet{}, oaNoTokenError("refresh returned no token", resp.body)
	}
	return ts, nil
}
