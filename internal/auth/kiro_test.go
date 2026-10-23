package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// Kiro 账号落盘 → LoadDir 读回：平台、路由域、token、专属字段全部保真；
// 与 WorkBuddy / Qoder 账号同目录共存，互不串味。
func TestKiroAuthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := NewKiro("k-1", "alice@example.com", "kiro-at", "kiro-rt", 1893456000, KiroMeta{
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       "us-east-1",
		ClientID:     "cid",
		ClientSecret: "csecret",
		ProfileArn:   "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABC",
		MachineID:    "m-1",
		Email:        "alice@example.com",
		Plan:         "PRO",
		NextResetAt:  1900000000,
		Source:       "builder_id",
	})
	a.FilePath = filepath.Join(dir, KiroFileName(a.UID))
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	q := NewQoder("q-1", "bob", "cn", "q-at", "q-rt", 1893456000, QoderMeta{})
	q.FilePath = filepath.Join(dir, QoderFileName(q.UID))
	if err := q.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
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
	if len(byUID) != 3 {
		t.Fatalf("loaded %d accounts, want 3", len(byUID))
	}
	k := byUID["k-1"]
	if k == nil || !k.IsKiro() || k.IsQoder() || k.IsWorkBuddy() || k.Platform() != PlatformKiro {
		t.Fatalf("kiro account lost its platform: %+v", k)
	}
	if k.Realm() != RealmKiro || !IsKiroRealm(k.Realm()) || IsQoderRealm(k.Realm()) {
		t.Errorf("realm=%q want %s", k.Realm(), RealmKiro)
	}
	if k.AccessTokenValue() != "kiro-at" || k.RefreshTokenValue() != "kiro-rt" || k.ExpiresAtValue() != 1893456000 {
		t.Errorf("tokens not preserved")
	}
	m := k.KiroMeta()
	if m.AuthMethod != "idc" || m.ClientSecret != "csecret" || m.ProfileArn == "" || m.Plan != "PRO" || m.NextResetAt != 1900000000 {
		t.Errorf("meta not preserved: %+v", m)
	}
	if w := byUID["wb-1"]; w == nil || !w.IsWorkBuddy() || w.IsKiro() {
		t.Fatalf("workbuddy account misdetected: %+v", w)
	}
	if qq := byUID["q-1"]; qq == nil || !qq.IsQoder() || qq.IsKiro() {
		t.Fatalf("qoder account misdetected: %+v", qq)
	}
}

// Kiro-Go 导出的单个账号对象改名为 kiro-<id>.json 放进 auth_dir 即可载入（主键 id、无 uid）。
func TestKiroFlatFileByName(t *testing.T) {
	dir := t.TempDir()
	flat := `{"id":"5f0c7f7e-1111-4222-8333-444455556666","email":"c@example.com","accessToken":"at-x","refreshToken":"rt-y",
		"clientId":"cid","clientSecret":"cs","authMethod":"idc","provider":"Enterprise","region":"eu-central-1",
		"expiresAt":1893456000,"machineId":"mid","profileArn":"arn:aws:codewhisperer:eu-central-1:123456789012:profile/P"}`
	if err := os.WriteFile(filepath.Join(dir, KiroFileName("x")), []byte(flat), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("LoadDir: %v %d", err, len(list))
	}
	a := list[0]
	if !a.IsKiro() || a.UID != "5f0c7f7e-1111-4222-8333-444455556666" || a.Nickname != "c@example.com" {
		t.Fatalf("flat kiro file misparsed: kiro=%v uid=%q nick=%q", a.IsKiro(), a.UID, a.Nickname)
	}
	if m := a.KiroMeta(); m.Region != "eu-central-1" || m.ClientID != "cid" || m.Provider != "Enterprise" {
		t.Errorf("meta: %+v", m)
	}
	// 同样内容放在 workbuddy-*.json 里不应被当成 Kiro。
	if b, err := Parse([]byte(flat)); err != nil || b.IsKiro() {
		t.Errorf("Parse without kiro file name must stay workbuddy: %v", err)
	}
}

// API Key 账号的扁平文件没有 accessToken 时用 kiroApiKey 兜底；BackfillRealm 不碰 Kiro 账号。
func TestKiroAPIKeyFlatAndBackfill(t *testing.T) {
	raw := []byte(`{"uid":"k-api","platform":"kiro","kiroApiKey":"ksk_abc","authMethod":"api_key","region":"us-east-1"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsKiro() || a.AccessTokenValue() != "ksk_abc" {
		t.Fatalf("api key account: kiro=%v at=%q", a.IsKiro(), a.AccessTokenValue())
	}
	if changed, _ := a.BackfillRealm(); changed || a.RealmStored() != "" {
		t.Fatal("BackfillRealm must not touch kiro accounts")
	}
	SetGlobalEnabled(false)
	defer SetGlobalEnabled(true)
	if a.Realm() != RealmKiro {
		t.Fatalf("kiro realm must ignore the global switch, got %q", a.Realm())
	}
}

func TestKiroMetaIsCopy(t *testing.T) {
	a := NewKiro("k2", "", "at", "rt", 0, KiroMeta{Plan: "FREE"})
	m := a.KiroMeta()
	m.Plan = "PRO"
	if a.KiroMeta().Plan != "FREE" {
		t.Fatal("KiroMeta must return a copy")
	}
	a.UpdateKiro(func(m *KiroMeta) { m.Plan = "PRO" })
	if a.KiroMeta().Plan != "PRO" {
		t.Fatal("UpdateKiro did not persist")
	}
	if (&Auth{}).KiroMeta() != (KiroMeta{}) {
		t.Fatal("non-kiro account must return zero meta")
	}
}
