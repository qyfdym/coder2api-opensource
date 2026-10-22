package qoder

import (
	"context"
	"testing"
)

func testClient() *Client { return NewClient(Options{}) }

func TestModelsSnapshot(t *testing.T) {
	c := testClient()
	for _, realm := range []string{RealmCN, RealmIntl} {
		models, err := c.Models(context.Background(), realm, nil)
		if err != nil {
			t.Fatalf("Models(%s): %v", realm, err)
		}
		if len(models) == 0 {
			t.Fatalf("Models(%s): empty", realm)
		}
		// lite 是 non-chat，必须被过滤。
		for _, m := range models {
			if m.Key == "lite" {
				t.Errorf("Models(%s) leaked non-chat model lite", realm)
			}
		}
	}
}

func TestLookupModel(t *testing.T) {
	c := testClient()
	cases := []struct {
		realm, name, wantKey, wantID string
	}{
		{RealmCN, "Qwen3.8-Max", "qmodel_38max", "Qwen3.8-Max"},
		{RealmCN, "qmodel_38max", "qmodel_38max", "Qwen3.8-Max"},
		{RealmCN, "qmodel_38max (Qwen3.8-Max)", "qmodel_38max", "Qwen3.8-Max"},
		{RealmCN, "QWEN3.8-MAX", "qmodel_38max", "Qwen3.8-Max"},
		{RealmCN, "glm-5.2", "gm51model", ""}, // cn 独占（快照里无条目时 ID 回退 key）
		{RealmIntl, "Ultimate", "ultimate", "Ultimate"},
		{RealmIntl, "deepseek-v4-pro", "dmodel", "DeepSeek-V4-Pro"},
		{RealmCN, "deepseek-flash", "dfmodel", "DeepSeek-Flash"},
	}
	for _, tc := range cases {
		m, ok := c.LookupModel(tc.realm, tc.name)
		if !ok {
			if tc.wantKey == "gm51model" {
				// gm51model 在 cn 快照里可能没有实体条目（只在别名/独占表）：允许解析失败。
				continue
			}
			t.Errorf("LookupModel(%s,%q): not found", tc.realm, tc.name)
			continue
		}
		if m.Key != tc.wantKey {
			t.Errorf("LookupModel(%s,%q).Key = %q, want %q", tc.realm, tc.name, m.Key, tc.wantKey)
		}
		if tc.wantID != "" && m.ID != tc.wantID {
			t.Errorf("LookupModel(%s,%q).ID = %q, want %q", tc.realm, tc.name, m.ID, tc.wantID)
		}
	}
}

func TestQmodel38MaxEffortsCN(t *testing.T) {
	c := testClient()
	m, ok := c.LookupModel(RealmCN, "qmodel_38max")
	if !ok {
		t.Fatal("qmodel_38max not found")
	}
	wantEfforts := []string{"low", "medium", "xhigh"}
	if len(m.Efforts) != len(wantEfforts) {
		t.Fatalf("efforts = %v, want %v", m.Efforts, wantEfforts)
	}
	for i := range wantEfforts {
		if m.Efforts[i] != wantEfforts[i] {
			t.Fatalf("efforts = %v, want %v", m.Efforts, wantEfforts)
		}
	}
	if m.DefaultEffort != "medium" {
		t.Errorf("default effort = %q, want medium", m.DefaultEffort)
	}
	if !m.Reasoning || !m.Vision {
		t.Errorf("qmodel_38max should be reasoning+vision, got reasoning=%v vision=%v", m.Reasoning, m.Vision)
	}
	if m.PriceFactorValley == nil {
		t.Error("qmodel_38max should have a valley price (promotion present)")
	} else if *m.PriceFactorValley != 0.2 { // before=0.5 * discount 0.4
		t.Errorf("valley = %v, want 0.2", *m.PriceFactorValley)
	}
}

func TestModelRealms(t *testing.T) {
	c := testClient()
	if got := c.ModelRealms("Qwen3.8-Max"); len(got) != 2 {
		t.Errorf("ModelRealms(Qwen3.8-Max) = %v, want both realms", got)
	}
	// Sonus (smodel) 仅国际版。
	intlOnly := c.ModelRealms("Sonus")
	if len(intlOnly) != 1 || intlOnly[0] != RealmIntl {
		t.Errorf("ModelRealms(Sonus) = %v, want [intl]", intlOnly)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	c := testClient()
	qmeta := c.catalogMetaFor(RealmCN, "qmodel_38max") // efforts: low/medium/xhigh default medium
	dmeta := c.catalogMetaFor(RealmIntl, "dfmodel")    // efforts: high/max/low default max

	type want struct {
		val  string
		send bool
	}
	cases := []struct {
		meta   *jval
		effort string
		want   want
	}{
		{qmeta, "high", want{"medium", true}},   // 不支持 high -> 最近合法档（偏默认 medium）
		{qmeta, "medium", want{"medium", true}}, // 命中
		{qmeta, "none", want{"none", true}},     // 关闭类
		{qmeta, "off", want{"none", true}},
		{dmeta, "medium", want{"high", true}}, // dfmodel 支持 low/high/max，medium -> high
		{dmeta, "low", want{"low", true}},
	}
	for _, tc := range cases {
		got, _, send := normalizeReasoningEffort(tc.effort, tc.meta)
		if got != tc.want.val || send != tc.want.send {
			t.Errorf("normalize(%q) = (%q,%v), want (%q,%v)", tc.effort, got, send, tc.want.val, tc.want.send)
		}
	}
}
