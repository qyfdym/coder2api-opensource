package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func qoderTestPool() *Pool {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range []*auth.Auth{
		{UID: "wb-cn", AccessToken: "at", ExpiresAt: 9999999999},
		{UID: "wb-gl", Domain: "www.workbuddy.ai", AccessToken: "at", ExpiresAt: 9999999999},
		auth.NewQoder("qd-cn", "", "cn", "at", "rt", 9999999999, auth.QoderMeta{}),
		auth.NewQoder("qd-intl", "", "intl", "at", "rt", 9999999999, auth.QoderMeta{}),
	} {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// 同一个池里两平台四个路由域互不串号：每个域只能选到本域账号。
func TestQoderRealmIsolation(t *testing.T) {
	p := qoderTestPool()
	for realm, want := range map[string]string{
		"cn":                "wb-cn",
		"global":            "wb-gl",
		auth.RealmQoderCN:   "qd-cn",
		auth.RealmQoderIntl: "qd-intl",
	} {
		tried := map[string]bool{}
		for i := 0; i < 4; i++ {
			a := p.PickExcludingForRealm(tried, "m", realm)
			if a == nil {
				break
			}
			if a.UID != want {
				t.Errorf("realm %s picked %s, want only %s", realm, a.UID, want)
			}
			tried[a.UID] = true
			p.Release(a.UID)
		}
		if got := p.AvailableUIDsForRealm(realm); len(got) != 1 || got[0] != want {
			t.Errorf("AvailableUIDsForRealm(%s)=%v want [%s]", realm, got, want)
		}
	}
	if st, _ := p.Status("qd-intl"); st.Platform != auth.PlatformQoder || st.Realm != auth.RealmQoderIntl {
		t.Errorf("status platform/realm = %q/%q", st.Platform, st.Realm)
	}
	if st, _ := p.Status("wb-cn"); st.Platform != auth.PlatformWorkBuddy {
		t.Errorf("wb status platform = %q", st.Platform)
	}
}

// Qoder 在途上限单独一档；0 = 跟随 max_in_flight。
func TestQoderInFlightLimit(t *testing.T) {
	p := qoderTestPool()
	p.SetMaxInFlight(3)
	p.SetMaxInFlightQoder(1)
	if !p.Acquire("qd-cn") {
		t.Fatal("first acquire must succeed")
	}
	if p.Acquire("qd-cn") {
		t.Fatal("qoder limit 1: second acquire must fail")
	}
	p.Release("qd-cn")
	// WorkBuddy 账号不受 Qoder 档影响。
	for i := 0; i < 3; i++ {
		if !p.Acquire("wb-cn") {
			t.Fatalf("wb acquire %d must succeed under max_in_flight=3", i+1)
		}
	}
	p.SetMaxInFlightQoder(0) // 跟随 max_in_flight=3
	for i := 0; i < 3; i++ {
		if !p.Acquire("qd-intl") {
			t.Fatalf("qoder acquire %d must succeed when following max_in_flight", i+1)
		}
	}
	if p.Acquire("qd-intl") {
		t.Fatal("qoder acquire beyond max_in_flight must fail")
	}
}
