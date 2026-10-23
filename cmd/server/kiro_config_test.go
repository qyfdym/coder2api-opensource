package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestKiroConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Kiro.Enabled || c.Kiro.Endpoint != "auto" || c.Kiro.HeaderTimeoutSeconds != 120 {
		t.Errorf("defaults: %+v", c.Kiro)
	}
	if c.Pool.Strategy.Kiro != "weighted_random" || c.Pool.MaxInFlightKiro != 0 || c.Pool.CreditFloorKiro != 0 {
		t.Errorf("pool kiro defaults: %+v / %d / %d", c.Pool.Strategy, c.Pool.MaxInFlightKiro, c.Pool.CreditFloorKiro)
	}
	if !slices.Contains(restartRequiredFields(c), "kiro") {
		t.Error("kiro section is wired at startup and must be restart-required")
	}
}

func TestKiroConfigNormalize(t *testing.T) {
	c, err := ParseConfig([]byte(`{"kiro":{"enabled":false,"endpoint":" CodeWhisperer ","proxy_url":" socks5://127.0.0.1:1080 ",
		"kiro_version":" 0.9.0 ","header_timeout_seconds":-3},
		"pool":{"max_in_flight_kiro":-1,"credit_floor_kiro":-5,"strategy":{"kiro":"sequential_exhaust"},"account_order":{"kiro":["k2","k1"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kiro.Enabled || c.Kiro.Endpoint != "codewhisperer" || c.Kiro.ProxyURL != "socks5://127.0.0.1:1080" ||
		c.Kiro.KiroVersion != "0.9.0" || c.Kiro.HeaderTimeoutSeconds != 120 {
		t.Errorf("normalize: %+v", c.Kiro)
	}
	if c.Pool.MaxInFlightKiro != 0 || c.Pool.CreditFloorKiro != 0 || c.Pool.Strategy.Kiro != "sequential_exhaust" {
		t.Errorf("pool: %d %d %q", c.Pool.MaxInFlightKiro, c.Pool.CreditFloorKiro, c.Pool.Strategy.Kiro)
	}
	if got := c.Pool.AccountOrder.Kiro; len(got) != 2 || got[0] != "k2" {
		t.Errorf("account_order.kiro=%v", got)
	}
	for _, bad := range []string{
		`{"kiro":{"endpoint":"bedrock"}}`,
		`{"kiro":{"proxy_url":"ftp://proxy.example"}}`,
		`{"pool":{"strategy":{"kiro":"lottery"}}}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

// 样例配置的 kiro 段与 pool 里的 Kiro 调度项按预期生效。
func TestExampleConfigKiroSection(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if !c.Kiro.Enabled || c.Kiro.Endpoint != "auto" || c.Pool.Strategy.Kiro != "weighted_random" {
		t.Errorf("kiro section: %+v strategy=%+v", c.Kiro, c.Pool.Strategy)
	}
}
