package livecfg

import "testing"

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - qoder: → qoder-cn + qoder-intl；qoder-cn: / qoder-intl: → 只藏该区
//   - 未知前缀（如 "xx:yy"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 非法前缀视为裸名整体
	got = NormalizeHiddenModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}

	// "qoder:X" → Qoder 两区都藏（handler 按 "<路由域>:<模型>" 查表），不波及 WorkBuddy。
	got = NormalizeHiddenModels([]string{"qoder:Qwen3.8-Max"})
	if len(got) != 2 {
		t.Errorf("qoder: prefix: got %d entries %v, want 2", len(got), got)
	}
	for _, want := range []string{"qoder-cn:Qwen3.8-Max", "qoder-intl:Qwen3.8-Max"} {
		if _, ok := got[want]; !ok {
			t.Errorf("qoder: prefix: missing %q in %v", want, got)
		}
	}
	// 显式 Qoder 区前缀只藏该区。
	got = NormalizeHiddenModels([]string{"qoder-intl:MiniMax-M3"})
	if _, ok := got["qoder-intl:MiniMax-M3"]; !ok || len(got) != 1 {
		t.Errorf("qoder-intl: prefix: got %v, want only qoder-intl:MiniMax-M3", got)
	}
	// 裸名不波及 Qoder。
	got = NormalizeHiddenModels([]string{"glm-5.2"})
	if _, ok := got["qoder-cn:glm-5.2"]; ok {
		t.Errorf("bare name must not hide qoder realms: %v", got)
	}
	// cn:/global: 前缀不额外保留原串（本地检查恒用带前缀键，原串就是规范键）。
	got = NormalizeHiddenModels([]string{"global:secret-model"})
	if len(got) != 1 {
		t.Errorf("global: prefix: got %d entries %v, want 1", len(got), got)
	}
}
