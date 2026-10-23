package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// fakeKiroOps 固定返回一组额度；exhaustCool 非 nil 时同时实现 KiroExhaustPolicy。
type fakeKiroOps struct {
	remain, total int64
	reset         time.Time
}

func (f fakeKiroOps) Keepalive(*auth.Auth) (bool, error) { return false, nil }
func (f fakeKiroOps) Balance(*auth.Auth) (int64, int64, time.Time, error) {
	return f.remain, f.total, f.reset, nil
}

// policyKiroOps 在 fakeKiroOps 上加「额度用完是否冷却」的策略（生产里由 kiroacct 的超额开关决定）。
type policyKiroOps struct {
	fakeKiroOps
	cool bool
}

func (p policyKiroOps) ShouldCoolOnExhausted(*auth.Auth, int64, int64) bool { return p.cool }

func kiroSched(t *testing.T, ops KiroOps) (*Scheduler, *pool.Pool, *auth.Auth) {
	t.Helper()
	pl := pool.New("")
	a := auth.NewKiro("k1", "kiro-1", "at", "rt", time.Now().Add(time.Hour).Unix(), auth.KiroMeta{AuthMethod: "social"})
	pl.Add(a)
	return New(Config{Pool: pl, Kiro: ops}), pl, a
}

// 额度用完且没有策略接口：沿用原行为，冷却到下次重置。
func TestKiroBalanceCoolsExhaustedByDefault(t *testing.T) {
	s, pl, a := kiroSched(t, fakeKiroOps{remain: 0, total: 100, reset: time.Now().Add(3 * time.Hour)})
	s.kiroBalance("k1", a)
	if st, _ := pl.Status("k1"); !st.Cooling || st.CoolKind != "hard_credit" {
		t.Fatalf("exhausted account must be cooled: %+v", st)
	}
}

// 实现了策略接口且判定「不冷却」（例如已打开超额）：额度用完也继续留在池里。
func TestKiroBalanceHonorsExhaustPolicy(t *testing.T) {
	s, pl, a := kiroSched(t, policyKiroOps{fakeKiroOps{remain: 0, total: 100, reset: time.Now().Add(3 * time.Hour)}, false})
	s.kiroBalance("k1", a)
	if st, _ := pl.Status("k1"); st.Cooling {
		t.Fatalf("policy said keep serving, but account was cooled: %+v", st)
	}
	// 策略说冷却：照常冷却。
	s2, pl2, a2 := kiroSched(t, policyKiroOps{fakeKiroOps{remain: 0, total: 100, reset: time.Now().Add(3 * time.Hour)}, true})
	s2.kiroBalance("k1", a2)
	if st, _ := pl2.Status("k1"); !st.Cooling {
		t.Fatalf("policy said cool: %+v", st)
	}
}

// 还有额度 / 查不到总额：任何策略下都不冷却；额度写进池快照。
func TestKiroBalanceNeverCoolsWithCredits(t *testing.T) {
	for name, ops := range map[string]KiroOps{
		"plain":      fakeKiroOps{remain: 40, total: 100},
		"policy":     policyKiroOps{fakeKiroOps{remain: 40, total: 100}, true},
		"zero total": fakeKiroOps{remain: 0, total: 0},
	} {
		s, pl, a := kiroSched(t, ops)
		s.kiroBalance("k1", a)
		st, _ := pl.Status("k1")
		if st.Cooling {
			t.Errorf("%s: account with credits (or unknown total) must not be cooled: %+v", name, st)
		}
		if name != "zero total" && (st.Credits != 40 || st.CreditsTotal != 100) {
			t.Errorf("%s: credits snapshot not recorded: %+v", name, st)
		}
	}
}
