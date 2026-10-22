package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseEffortFromBody(t *testing.T, body []byte) string {
	t.Helper()
	var obj struct {
		Effort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("prepared body not json: %v", err)
	}
	return strings.ToLower(obj.Effort)
}

// OutboundEffort 必须与 prepareBody 实际出站的 reasoning_effort 一致（同一套补默认 + 降级规则）。
func TestOutboundEffortMatchesPreparedBody(t *testing.T) {
	c := New()
	c.storeEfforts("cn", map[string][]string{
		"deepseek-v4.1-flash": {"low", "high", "max"},
		"glm-5.3":             {"low", "medium", "high"},
	}, map[string]string{"deepseek-v4.1-flash": "high"})

	for _, tc := range []struct {
		name  string
		model string
		req   EffortRequest
		body  string
		want  string
	}{
		{"deepseek no effort → default", "deepseek-v4.1-flash", EffortRequest{}, `{"model":"deepseek-v4.1-flash","messages":[]}`, "high"},
		{"deepseek medium → downgraded", "deepseek-v4.1-flash", EffortRequest{Effort: "medium"}, `{"model":"deepseek-v4.1-flash","reasoning_effort":"medium","messages":[]}`, "low"},
		{"deepseek thinking disabled", "deepseek-v4.1-flash", EffortRequest{ThinkingType: "disabled"}, `{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"messages":[]}`, "off"},
		{"glm xhigh → high", "glm-5.3", EffortRequest{Effort: "xhigh"}, `{"model":"glm-5.3","reasoning_effort":"xhigh","messages":[]}`, "high"},
		{"glm no effort → none sent", "glm-5.3", EffortRequest{}, `{"model":"glm-5.3","messages":[]}`, ""},
		{"unknown model passthrough", "kimi-k3", EffortRequest{Effort: "Max"}, `{"model":"kimi-k3","reasoning_effort":"Max","messages":[]}`, "max"},
	} {
		got := c.OutboundEffort("cn", tc.model, tc.req)
		if got != tc.want {
			t.Errorf("%s: OutboundEffort=%q want %q", tc.name, got, tc.want)
		}
		// 与真实出站体对照（off 是展示口径，出站体里是 thinking.disabled + 无 effort）。
		out := c.prepareBody([]byte(tc.body), "cn", "u1", "")
		sent := parseEffortFromBody(t, out)
		want := tc.want
		if want == "off" {
			want = ""
		}
		if sent != want {
			t.Errorf("%s: prepared body effort=%q, OutboundEffort said %q", tc.name, sent, tc.want)
		}
	}
}

// global 域走产品静态兜底表：deepseek-v4.1-flash 国际版只认 high。
func TestOutboundEffortGlobalStaticTable(t *testing.T) {
	c := New()
	if got := c.OutboundEffort("global", "deepseek-v4.1-flash", EffortRequest{Effort: "max"}); got != "high" {
		t.Fatalf("global deepseek max → %q want high", got)
	}
}
