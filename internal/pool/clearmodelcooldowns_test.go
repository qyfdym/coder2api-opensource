package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ClearModelCooldowns 只清这个账号的模型级冷却：其它账号的、账号级状态（暂停）都不受影响。
func TestClearModelCooldownsOnlyTouchesThatAccountsModels(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.BlockModelBackoff("u1", "claude-sonnet-4.5", "11102 model unavailable")
	p.BlockModelBackoff("u2", "claude-sonnet-4.5", "11102 model unavailable")
	if st := p.ModelBlocked("claude-sonnet-4.5"); !st.Blocked || st.Count != 2 {
		t.Fatalf("precondition: %+v", st)
	}

	if !p.ClearModelCooldowns("u1") {
		t.Fatal("known account must return true")
	}
	if st, _ := p.Status("u1"); len(st.RateLimitedModels) != 0 {
		t.Errorf("u1 must be cleared: %+v", st.RateLimitedModels)
	}
	if st, _ := p.Status("u2"); len(st.RateLimitedModels) != 1 {
		t.Errorf("u2 must keep its block: %+v", st.RateLimitedModels)
	}
	if st := p.ModelBlocked("claude-sonnet-4.5"); st.Blocked {
		t.Errorf("u1 can serve the model again, the pool is no longer blocked: %+v", st)
	}

	// 暂停是账号级状态，清模型冷却不能顺手把它解除。
	p.Pause("u1")
	p.BlockModelBackoff("u1", "claude-opus-4.6", "11102 model unavailable")
	p.ClearModelCooldowns("u1")
	if st, _ := p.Status("u1"); !st.Paused || len(st.RateLimitedModels) != 0 {
		t.Errorf("paused must stay paused and models cleared: paused=%v models=%v", st.Paused, st.RateLimitedModels)
	}
	if p.ClearModelCooldowns("nobody") {
		t.Errorf("unknown account must return false")
	}
}
