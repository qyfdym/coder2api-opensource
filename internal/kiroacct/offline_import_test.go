package kiroacct

// 批量导入在服务器连不上 Kiro 时：凭证没被拒绝就先保存并给出提示；凭证被拒绝仍然失败。

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/kiro"
)

// unreachableRT 模拟服务器到 Kiro 的出口不通（拨号失败）。
type unreachableRT struct{}

func (unreachableRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
}

func offlineItem() kiro.ImportItem {
	return kiro.ImportItem{
		AccessToken: "old-access", RefreshToken: "the-refresh-token", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
		AuthMethod: "idc", Provider: "BuilderId", Region: "us-east-1",
		ClientID: "cid", ClientSecret: "csecret", Email: "me@example.com", UserID: "user-offline",
		ProfileArn: testArn,
	}
}

func TestImportSavesOfflineWhenKiroUnreachable(t *testing.T) {
	s := &Service{
		Client:  kiro.NewClient(kiro.Options{Transport: unreachableRT{}, Logf: func(string, ...any) {}}),
		AuthDir: t.TempDir(),
	}
	res := s.Import(context.Background(), []kiro.ImportItem{offlineItem()})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("import must succeed offline: %+v", res)
	}
	if !strings.Contains(res[0].Warning, "没能联网校验") {
		t.Errorf("an unverified import must say so: %q", res[0].Warning)
	}
	a := loadOne(t, s.AuthDir)
	m := a.KiroMeta()
	if a.UID != kiro.StableUID("user-offline") || a.RefreshTokenValue() != "the-refresh-token" || m.ClientID != "cid" ||
		m.Email != "me@example.com" || m.ProfileArn != testArn || m.MachineID == "" {
		t.Errorf("saved account: uid=%s meta=%+v", a.UID, m)
	}

	// 登录流程（建档前必须先拿到 token）不走这条宽松路径。
	if _, err := s.createAccount(context.Background(), offlineItem(), "import", true); err == nil {
		t.Errorf("createAccount without offlineOK must still fail when Kiro is unreachable")
	}
}

// 上游明确拒绝凭证（refresh token 失效）不能当成「连不上」放过去。
func TestImportStillRejectsRefusedCredentials(t *testing.T) {
	f := newFakeRT()
	f.on(routeOIDC, func(*http.Request, []byte) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"Invalid refresh token provided"}`
	})
	s := newService(t, f)
	res := s.Import(context.Background(), []kiro.ImportItem{offlineItem()})
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "刷新 token 失败") {
		t.Fatalf("a refused credential must fail the import: %+v", res)
	}
	if list, err := auth.LoadDir(s.AuthDir); err != nil || len(list) != 0 {
		t.Errorf("nothing may be saved for a refused credential: %d accounts, err=%v", len(list), err)
	}
}
