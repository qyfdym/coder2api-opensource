package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// Qoder 账号落盘 → LoadDir 读回：平台、区域、路由域、token、专属字段全部保真；
// 与 WorkBuddy 账号同目录共存，互不串味。
func TestQoderAuthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := NewQoder("u-qd-1", "alice", "intl", "dt-access", "drt-refresh", 1893456000, QoderMeta{
		PAT:                  "pt-secret",
		UserType:             "personal_standard",
		Plan:                 "Pro",
		Source:               "oauth",
		AddedAt:              1700000000,
		LastCheckin:          "2026-10-20 10:05:00",
		CheckinNote:          "已到账 +50",
		CampaignCode:         map[string]QoderCampaignCode{"c1": {Campaign: "双十一", Code: "CODE-1", At: 1700000001}},
		CampaignBlockedUntil: map[string]int64{"c2": 1900000000},
	})
	a.FilePath = filepath.Join(dir, QoderFileName(a.UID))
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	// 同目录放一个 WorkBuddy 老凭证（无 platform 字段）。
	wb := `{"accessToken":"wb-at","refreshToken":"wb-rt","uid":"wb-1","expiresAt":1893456000}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-wb-1.json"), []byte(wb), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]*Auth{}
	for _, x := range list {
		byUID[x.UID] = x
	}
	if len(byUID) != 2 {
		t.Fatalf("loaded %d accounts, want 2", len(byUID))
	}

	q := byUID["u-qd-1"]
	if q == nil || !q.IsQoder() || q.IsWorkBuddy() || q.Platform() != PlatformQoder {
		t.Fatalf("qoder account lost its platform: %+v", q)
	}
	if q.QoderRegion() != "intl" || q.Realm() != RealmQoderIntl {
		t.Errorf("region=%q realm=%q want intl / %s", q.QoderRegion(), q.Realm(), RealmQoderIntl)
	}
	if q.AccessTokenValue() != "dt-access" || q.RefreshTokenValue() != "drt-refresh" || q.ExpiresAtValue() != 1893456000 {
		t.Errorf("tokens not preserved")
	}
	m := q.QoderMeta()
	if m.PAT != "pt-secret" || m.Plan != "Pro" || m.LastCheckin != "2026-10-20 10:05:00" || m.CheckinNote != "已到账 +50" {
		t.Errorf("meta not preserved: %+v", m)
	}
	if m.CampaignCode["c1"].Code != "CODE-1" || m.CampaignBlockedUntil["c2"] != 1900000000 {
		t.Errorf("campaign state not preserved: %+v", m)
	}

	w := byUID["wb-1"]
	if w == nil || w.IsQoder() || w.Platform() != PlatformWorkBuddy {
		t.Fatalf("workbuddy account misdetected: %+v", w)
	}
	if IsQoderRealm(w.Realm()) {
		t.Errorf("workbuddy realm=%q must not be a qoder realm", w.Realm())
	}
	if w.QoderRegion() != "" {
		t.Errorf("workbuddy QoderRegion=%q want empty", w.QoderRegion())
	}
}

// qoder2api-hub 的扁平账号文件改名为 qoder-<uid>.json 放进 auth_dir 即可载入：文件名决定平台
// （原实现的 platform 字段是客户端形态 "CLI"，不能拿来判平台）。
func TestQoderFlatFileByName(t *testing.T) {
	dir := t.TempDir()
	flat := `{"uid":"u-flat","nickname":"bob","realm":"intl","platform":"CLI","accessToken":"dt-x","refreshToken":"drt-y",
		"expiresAt":1893456000,"personalToken":"pt-z","userType":"personal_pro","plan":"Pro","lastCheckin":"2026-10-21 15:29:32"}`
	if err := os.WriteFile(filepath.Join(dir, QoderFileName("u-flat")), []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("LoadDir: %v %d", err, len(list))
	}
	a := list[0]
	if !a.IsQoder() || a.Realm() != RealmQoderIntl || a.RefreshTokenValue() != "drt-y" {
		t.Fatalf("flat qoder file misparsed: qoder=%v realm=%q", a.IsQoder(), a.Realm())
	}
	if m := a.QoderMeta(); m.PAT != "pt-z" || m.Plan != "Pro" || m.LastCheckin != "2026-10-21 15:29:32" {
		t.Errorf("meta: %+v", m)
	}
	// 同样内容放在 workbuddy-*.json 里不应被当成 Qoder。
	if b, err := Parse([]byte(flat)); err != nil || b.IsQoder() {
		t.Errorf("Parse without qoder file name must stay workbuddy: %v %v", err, b.IsQoder())
	}
}

// Qoder 路由域不受 global 逃生门影响（逃生门只锁 WorkBuddy 的 global 端点）。
func TestQoderRealmIgnoresGlobalSwitch(t *testing.T) {
	SetGlobalEnabled(false)
	defer SetGlobalEnabled(true)
	a := NewQoder("u2", "", "intl", "at", "rt", 0, QoderMeta{})
	if a.Realm() != RealmQoderIntl {
		t.Fatalf("realm=%q want %s", a.Realm(), RealmQoderIntl)
	}
	if NewQoder("u3", "", "", "at", "rt", 0, QoderMeta{}).Realm() != RealmQoderCN {
		t.Fatal("empty region must default to qoder-cn")
	}
}

// QoderMeta() 返回副本：调用方改动不得泄漏回账号（并发读写安全的前提）。
func TestQoderMetaIsCopy(t *testing.T) {
	a := NewQoder("u4", "", "cn", "at", "rt", 0, QoderMeta{CampaignCode: map[string]QoderCampaignCode{"x": {Code: "A"}}})
	m := a.QoderMeta()
	m.CampaignCode["x"] = QoderCampaignCode{Code: "B"}
	if a.QoderMeta().CampaignCode["x"].Code != "A" {
		t.Fatal("QoderMeta must return a deep copy")
	}
	a.UpdateQoder(func(m *QoderMeta) { m.Plan = "Pro" })
	if a.QoderMeta().Plan != "Pro" {
		t.Fatal("UpdateQoder did not persist")
	}
}
