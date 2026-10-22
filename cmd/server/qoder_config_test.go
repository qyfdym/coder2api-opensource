package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestQoderConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{"state_file":"./data/state.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Qoder.Enabled || c.Qoder.DefaultRealm != "cn" {
		t.Errorf("defaults: enabled=%v realm=%q", c.Qoder.Enabled, c.Qoder.DefaultRealm)
	}
	if c.Qoder.HeaderTimeoutSeconds != 120 || c.Qoder.QueueMaxWaitSeconds != 600 {
		t.Errorf("timeouts: header=%d queue=%d", c.Qoder.HeaderTimeoutSeconds, c.Qoder.QueueMaxWaitSeconds)
	}
	if got, want := c.QoderDataDir(), filepath.Join("data", "qoder"); got != want {
		t.Errorf("QoderDataDir=%q want %q", got, want)
	}
	if c.Pool.Strategy.Qoder != "weighted_random" || c.Pool.MaxInFlightQoder != 0 || c.Pool.CreditFloorQoder != 0 {
		t.Errorf("pool qoder defaults: %+v / %d / %d", c.Pool.Strategy, c.Pool.MaxInFlightQoder, c.Pool.CreditFloorQoder)
	}
	if !c.Pool.CrossPlatformFallback {
		t.Error("pool.cross_platform_fallback should default to true")
	}
}

// 跨平台回退：显式 false 能关；models.fallback 映射原样解析（含禁止回退的空值）。
func TestCrossPlatformFallbackConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`{"pool":{"cross_platform_fallback":false},
		"models":{"fallback":{"cn:deepseek-v4.1-flash":"qoder:DeepSeek-Flash","glm-5.2":""}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CrossPlatformFallback {
		t.Error("explicit false must disable cross-platform fallback")
	}
	if c.Models.Fallback["cn:deepseek-v4.1-flash"] != "qoder:DeepSeek-Flash" {
		t.Errorf("fallback mapping: %+v", c.Models.Fallback)
	}
	if v, ok := c.Models.Fallback["glm-5.2"]; !ok || v != "" {
		t.Errorf("empty mapping value must be kept (= no fallback): %q %v", v, ok)
	}
}

func TestQoderConfigNormalize(t *testing.T) {
	c, err := ParseConfig([]byte(`{"qoder":{"enabled":false,"default_realm":"Global","data_dir":" /x/qd ",
		"intl_gateway":"https://api3.qoder.sh/"},"pool":{"max_in_flight_qoder":-1,"credit_floor_qoder":-5,
		"strategy":{"qoder":"round_robin"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Qoder.Enabled || c.Qoder.DefaultRealm != "intl" || c.QoderDataDir() != "/x/qd" {
		t.Errorf("normalize: %+v", c.Qoder)
	}
	if c.Qoder.IntlGateway != "https://api3.qoder.sh" {
		t.Errorf("intl_gateway=%q", c.Qoder.IntlGateway)
	}
	if c.Pool.MaxInFlightQoder != 0 || c.Pool.CreditFloorQoder != 0 || c.Pool.Strategy.Qoder != "round_robin" {
		t.Errorf("pool: %d %d %q", c.Pool.MaxInFlightQoder, c.Pool.CreditFloorQoder, c.Pool.Strategy.Qoder)
	}

	for _, bad := range []string{
		`{"qoder":{"default_realm":"mars"}}`,
		`{"qoder":{"intl_gateway":"http://plain.example"}}`,
		`{"pool":{"strategy":{"qoder":"lottery"}}}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

// 仓库自带的样例配置必须能被网关加载，且 Qoder 段按预期生效（Docker 镜像默认就用它）。
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if !c.Qoder.Enabled || c.Qoder.DefaultRealm != "cn" || c.Pool.Strategy.Qoder != "weighted_random" {
		t.Errorf("qoder section: %+v strategy=%+v", c.Qoder, c.Pool.Strategy)
	}
}

// 旧配置里遗留的 panel.qoder / upstream.passthrough 段被安静忽略（未知键），不阻塞启动。
func TestQoderLegacyKeysIgnored(t *testing.T) {
	raw := `{"panel":{"qoder":{"enabled":true,"base_url":"http://127.0.0.1:8790"}},
		"upstream":{"passthrough":[{"name":"qoder","prefix":"qoder:","base_url":"http://127.0.0.1:8790"}]}}`
	c, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("legacy keys must not break parsing: %v", err)
	}
	if !c.Qoder.Enabled {
		t.Error("qoder should stay enabled by default")
	}
	if fields := strings.Join(restartRequiredFields(c), ","); !strings.Contains(fields, "qoder") {
		t.Errorf("restart-required fields must include qoder: %s", fields)
	}
}
