package kiroacct

// 模型目录按账号授权返回（学生号没有 Claude），全池目录是各账号目录的并集。

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const routeModels = "codewhisperer.us-east-1.amazonaws.com/ListAvailableModels"

func catalogAcct(uid, access string, expiresIn time.Duration) *auth.Auth {
	return auth.NewKiro(uid, uid, access, "rt-"+uid, time.Now().Add(expiresIn).Unix(), auth.KiroMeta{
		AuthMethod: "idc", Provider: "BuilderId", Region: "us-east-1",
	})
}

// catalogRT 按 Authorization 返回不同目录的假上游。
func catalogRT() *fakeRT {
	f := newFakeRT()
	f.on(routeModels, func(r *http.Request, _ []byte) (int, string) {
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "at-student":
			return 200, `{"models":[{"modelId":"qwen3-coder-next","modelName":"Qwen"},{"modelId":"gpt-5.6-luna","modelName":"GPT"}]}`
		case "at-pro":
			return 200, `{"models":[{"modelId":"qwen3-coder-next","modelName":"Qwen"},{"modelId":"claude-sonnet-4.5","modelName":"Sonnet"}]}`
		}
		return 403, `{"message":"denied"}`
	})
	return f
}

func modelIDs(s *Service) []string {
	var ids []string
	for _, m := range s.Client.CachedModels() {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestRefreshCatalogsUnionOfAccounts(t *testing.T) {
	f := catalogRT()
	s := newService(t, f)
	student, pro := catalogAcct("student", "at-student", time.Hour), catalogAcct("pro", "at-pro", time.Hour)

	if errs := s.RefreshCatalogs(context.Background(), []*auth.Auth{student, pro}, CatalogOpts{}); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := strings.Join(modelIDs(s), ",")
	if got != "claude-sonnet-4.5,gpt-5.6-luna,qwen3-coder-next" {
		t.Errorf("union catalog = %s", got)
	}
	if f.count(routeModels) != 2 {
		t.Errorf("fetches = %d, want 2", f.count(routeModels))
	}
	// 目录还新鲜：不重拉。强制时才重拉。
	s.RefreshCatalogs(context.Background(), []*auth.Auth{student, pro}, CatalogOpts{})
	if f.count(routeModels) != 2 {
		t.Errorf("fresh catalogs refetched: %d", f.count(routeModels))
	}
	s.RefreshCatalogs(context.Background(), []*auth.Auth{student, pro}, CatalogOpts{Force: true})
	if f.count(routeModels) != 4 {
		t.Errorf("forced refresh: %d fetches, want 4", f.count(routeModels))
	}
}

// 单次最多拉 Max 个账号，目录缺失的优先；剩下的下一次补上，已有的目录不会被丢掉。
func TestRefreshCatalogsLimitAndMissingFirst(t *testing.T) {
	f := catalogRT()
	s := newService(t, f)
	student, pro := catalogAcct("student", "at-student", time.Hour), catalogAcct("pro", "at-pro", time.Hour)

	s.RefreshCatalogs(context.Background(), []*auth.Auth{student, pro}, CatalogOpts{Max: 1})
	if f.count(routeModels) != 1 || strings.Contains(strings.Join(modelIDs(s), ","), "claude") {
		t.Fatalf("first pass: fetches=%d models=%v", f.count(routeModels), modelIDs(s))
	}
	s.RefreshCatalogs(context.Background(), []*auth.Auth{student, pro}, CatalogOpts{Max: 1})
	if f.count(routeModels) != 2 || !strings.Contains(strings.Join(modelIDs(s), ","), "claude-sonnet-4.5") ||
		!strings.Contains(strings.Join(modelIDs(s), ","), "gpt-5.6-luna") {
		t.Fatalf("second pass: fetches=%d models=%v", f.count(routeModels), modelIDs(s))
	}
}

// token 将过期的账号：不要求刷新时直接跳过（不为 /v1/models 拖慢响应），要求时才刷新。
func TestRefreshCatalogsSkipsExpiringWithoutEnsureFresh(t *testing.T) {
	f := catalogRT()
	s := newService(t, f)
	expiring := catalogAcct("student", "at-student", 10*time.Second)

	if errs := s.RefreshCatalogs(context.Background(), []*auth.Auth{expiring}, CatalogOpts{}); len(errs) != 0 || f.count(routeModels) != 0 {
		t.Errorf("expiring account must be skipped silently: errs=%v fetches=%d", errs, f.count(routeModels))
	}
}

// 一个账号拉不到目录：返回原因，不影响其它账号并入。
func TestRefreshCatalogsReportsFailureAndKeepsOthers(t *testing.T) {
	f := catalogRT()
	s := newService(t, f)
	bad, pro := catalogAcct("bad", "at-unknown", time.Hour), catalogAcct("pro", "at-pro", time.Hour)

	errs := s.RefreshCatalogs(context.Background(), []*auth.Auth{bad, pro}, CatalogOpts{})
	if len(errs) != 1 || !strings.Contains(errs[0], "bad") {
		t.Errorf("errors = %v", errs)
	}
	if got := strings.Join(modelIDs(s), ","); got != "claude-sonnet-4.5,qwen3-coder-next" {
		t.Errorf("catalog = %s", got)
	}
}
