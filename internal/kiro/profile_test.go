package kiro

// 1.3.x 版本号下推理与模型目录必须带 profileArn（缺了推理返回 "profileArn is required"）。
// 这些用例钉住：Builder ID / 社交登录账号没存 profileArn 时用固定值，企业账号不凭空编造。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const chatBody = `{"model":"qwen3-coder-next","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestDefaultProfileArn(t *testing.T) {
	for _, arn := range []string{builderIDProfileArn, socialProfileArn} {
		if _, region, ok := ParseProfileArn(arn); !ok || region != "us-east-1" {
			t.Errorf("fixed ARN %q must be a valid us-east-1 profile ARN", arn)
		}
	}
	own := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/OWN"
	cases := []struct {
		name string
		cred Cred
		want string
	}{
		{"builder id", Cred{AuthMethod: "idc", Provider: "BuilderId"}, builderIDProfileArn},
		{"google", Cred{AuthMethod: "social", Provider: "Google"}, socialProfileArn},
		{"github", Cred{AuthMethod: "social", Provider: "Github"}, socialProfileArn},
		{"own profile wins", Cred{AuthMethod: "idc", Provider: "BuilderId", ProfileArn: own}, own},
		{"enterprise has none", Cred{AuthMethod: "idc", Provider: "Enterprise"}, ""},
		{"external idp has none", Cred{AuthMethod: AuthMethodExternalIDP, Provider: ProviderAzureAD}, ""},
		{"api key has none", Cred{AuthMethod: "api_key", APIKey: "ksk_x", ProfileArn: own}, ""},
	}
	for _, cs := range cases {
		if got := effectiveProfileArn(cs.cred); got != cs.want {
			t.Errorf("%s: effectiveProfileArn = %q, want %q", cs.name, got, cs.want)
		}
	}
}

// 账号文件里没有 profileArn 的 Builder ID / 社交账号：推理请求体带固定 profileArn；
// idc 账号带 TokenType: SSO_OIDC，社交账号不带；企业账号没有 profile 时不编造。
func TestChatStreamSendsDefaultProfileArn(t *testing.T) {
	cases := []struct {
		name      string
		cred      Cred
		wantArn   string
		wantToken string
	}{
		{"builder id", Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}, builderIDProfileArn, "SSO_OIDC"},
		{"social", Cred{UID: "s1", AccessToken: "at", AuthMethod: "social", Provider: "Google"}, socialProfileArn, ""},
		{"enterprise", Cred{UID: "e1", AccessToken: "at", AuthMethod: "idc", Provider: "Enterprise"}, "", "SSO_OIDC"},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			f := newFake()
			f.on(routeIDE, streamResp(
				evt("assistantResponseEvent", map[string]any{"content": "OK"}),
				evt("metadataEvent", map[string]any{"stopReason": "end_turn"}),
			))
			rc, err := testClient(f).ChatStream(context.Background(), cs.cred, []byte(chatBody))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			readAll(t, rc)
			calls := f.calls(routeIDE)
			if len(calls) != 1 {
				t.Fatalf("upstream calls: %d", len(calls))
			}
			var sent map[string]any
			_ = json.Unmarshal(calls[0].Body, &sent)
			gotArn, _ := sent["profileArn"].(string)
			if gotArn != cs.wantArn {
				t.Errorf("profileArn = %q, want %q", gotArn, cs.wantArn)
			}
			if got := calls[0].Header.Get("TokenType"); got != cs.wantToken {
				t.Errorf("TokenType = %q, want %q", got, cs.wantToken)
			}
		})
	}
}

func TestListModelsSendsProfileArn(t *testing.T) {
	f := newFake()
	var query url.Values
	f.on(hostCW+"/ListAvailableModels", func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		query = r.URL.Query()
		return 200, nil, []byte(`{"models":[{"modelId":"qwen3-coder-next","modelName":"Qwen3 Coder Next","rateMultiplier":0.05}]}`)
	})
	cred := Cred{UID: "b1", AccessToken: "at", AuthMethod: "idc", Provider: "BuilderId"}
	models, err := testClient(f).ListModels(context.Background(), cred)
	if err != nil || len(models) != 1 || models[0].ID != "qwen3-coder-next" {
		t.Fatalf("ListModels: %+v %v", models, err)
	}
	if query.Get("profileArn") != builderIDProfileArn {
		t.Errorf("profileArn query = %q, want %q", query.Get("profileArn"), builderIDProfileArn)
	}
}

func TestUsageLimitsSendsProfileArn(t *testing.T) {
	f := newFake()
	var query url.Values
	f.on(routeUsage, func(r *http.Request, _ []byte) (int, http.Header, []byte) {
		query = r.URL.Query()
		return 200, nil, []byte(`{}`)
	})
	cred := Cred{UID: "s1", AccessToken: "at", AuthMethod: "social", Provider: "Google"}
	if _, err := testClient(f).UsageLimits(context.Background(), cred); err != nil {
		t.Fatalf("UsageLimits: %v", err)
	}
	if query.Get("profileArn") != socialProfileArn {
		t.Errorf("profileArn query = %q, want %q", query.Get("profileArn"), socialProfileArn)
	}
}

// ListAvailableProfiles 的请求体里不能有 maxResults（真机：带了会被 REQUEST_BODY_INVALID 拒绝）。
func TestListProfilesBodyHasNoMaxResults(t *testing.T) {
	f := newFake()
	var bodies []string
	f.on(hostCW+"/ListAvailableProfiles", func(_ *http.Request, body []byte) (int, http.Header, []byte) {
		bodies = append(bodies, string(body))
		return 200, nil, []byte(`{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/ENT","profileName":"ent"}]}`)
	})
	cred := Cred{UID: "e1", AccessToken: "at", AuthMethod: "idc", Provider: "Enterprise"}
	arn, err := testClient(f).ResolveProfileArn(context.Background(), cred)
	if err != nil || arn != "arn:aws:codewhisperer:us-east-1:123456789012:profile/ENT" {
		t.Fatalf("ResolveProfileArn: %q %v", arn, err)
	}
	if len(bodies) == 0 || strings.Contains(bodies[0], "maxResults") {
		t.Errorf("ListAvailableProfiles body = %v, must not carry maxResults", bodies)
	}
}

// 默认 User-Agent 里的版本号必须是当前 Kiro IDE：上游按它决定模型目录。
func TestDefaultKiroVersionInUserAgent(t *testing.T) {
	ua, amzUA := testClient(newFake()).userAgents(Cred{MachineID: "mid"}, "codewhispererstreaming", streamingSDKVersion, "m/E")
	if !strings.HasSuffix(ua, " KiroIDE-1.3.11-mid") || !strings.HasSuffix(amzUA, " KiroIDE-1.3.11-mid") {
		t.Errorf("user agents: %q / %q", ua, amzUA)
	}
}
