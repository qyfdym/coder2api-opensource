package server

// anthropic.go Anthropic Messages 兼容接口：
//
//	POST /v1/messages               对话（流式 / 非流式）
//	POST /v1/messages/count_tokens  输入 token 估算
//	GET  /v1/models                 带 anthropic-version 头（或只带 x-api-key）时返回 Anthropic 形态
//	GET  /v1/models/{id}            单个模型
//
// 给只会说 Anthropic 协议的客户端（Claude Desktop、Claude Code、Anthropic SDK）直连网关用。
// 请求先翻成 OpenAI chat.completions（anthropic_convert.go），交给 chatCompletions 走原有的
// 选号 / 轮换 / 冷却 / 回退 / 粘性 / 记账，输出再翻回 Anthropic 形态（anthropic_stream.go）。
//
// 模型名：claude-* 裸名（含 claude-sonnet-4-5、带日期快照、[1m] 后缀、anthropic/ 前缀）在池里
// 有 Kiro 账号时走 Kiro；cn: / global: / qoder: / kiro: 显式前缀照旧。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// isAnthropicClient 请求来自 Anthropic 协议客户端（SDK 恒带 anthropic-version；只带 x-api-key 不带
// Authorization 的也按 Anthropic 处理）。
func isAnthropicClient(r *http.Request) bool {
	if r.Header.Get("anthropic-version") != "" {
		return true
	}
	return r.Header.Get("x-api-key") != "" && r.Header.Get("Authorization") == ""
}

// anthropicPath /v1/messages 系列路径（鉴权失败时按 Anthropic 形态回错误）。
func anthropicPath(path string) bool {
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}

// anthropicRouteModel Anthropic 客户端发来的模型名 → 网关模型名。
func (h *Handler) anthropicRouteModel(model string) string {
	m := strings.TrimSpace(model)
	// Claude Code 的上下文档位后缀（claude-sonnet-4-5[1m]）：上游不认，去掉。
	if i := strings.LastIndexByte(m, '['); i > 0 && strings.HasSuffix(m, "]") {
		m = strings.TrimSpace(m[:i])
	}
	m = strings.TrimPrefix(m, "anthropic/")
	if _, _, explicit := resolveModelDetail(m); explicit {
		return m
	}
	if h.cfg.Kiro != nil && h.cfg.Kiro.Client != nil && h.realmHasAccounts(auth.RealmKiro) {
		if _, ok := h.cfg.Kiro.Client.LookupModel(m); ok || strings.HasPrefix(strings.ToLower(m), "claude") {
			return auth.RealmKiro + ":" + m
		}
	}
	return m
}

// anthropicMessages POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	req, ok := readAnthropicRequest(w, r)
	if !ok {
		return
	}
	routed := h.anthropicRouteModel(req.Model)
	// reject 请求在本地就被拒（没走到选号）：请求记录里写上模型与原因，否则面板只显示「未选到账号」。
	reject := func(msg string) {
		if tr := requestTraceFrom(r); tr != nil {
			tr.model = routed
			tr.note = "Anthropic 请求格式不被网关接受（未发往上游）：" + msg
		}
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", msg)
	}
	if len(req.Messages) == 0 {
		reject("messages: at least one message is required")
		return
	}
	body, err := anthropicToOpenAI(req, routed)
	if err != nil {
		reject(err.Error())
		return
	}
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Del("Content-Encoding")

	aw := newAnthropicWriter(w, req.Stream, req.Model, estimateAnthropicTokens(req))
	h.chatCompletions(aw, inner)
	aw.finish(r.Context())
}

// anthropicCountTokens POST /v1/messages/count_tokens（本地估算，不打上游）。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	req, ok := readAnthropicRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateAnthropicTokens(req)})
}

func readAnthropicRequest(w http.ResponseWriter, r *http.Request) (*anthropicRequest, bool) {
	fail := func(msg string) (*anthropicRequest, bool) {
		if tr := requestTraceFrom(r); tr != nil {
			tr.note = "Anthropic 请求格式不被网关接受（未发往上游）：" + msg
		}
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return nil, false
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return fail("failed to read request body: " + err.Error())
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fail("request body is not valid JSON: " + err.Error())
	}
	if strings.TrimSpace(req.Model) == "" {
		return fail("model: field required")
	}
	return &req, true
}

// ---------------------------------------------------------------------------
// 模型列表
// ---------------------------------------------------------------------------

// claudeDotVersion Kiro 目录的点号版本写法（claude-sonnet-4.5）。Anthropic 客户端用横线写法
// （claude-sonnet-4-5），列表按客户端习惯给；发回来时 kiro.ParseModel 再归一回点号。
var claudeDotVersion = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-(\d+)\.(\d{1,2})\b`)

// anthropicModelID Kiro 模型 id → Anthropic 客户端看到的 id。
func anthropicModelID(kiroID string) string {
	return claudeDotVersion.ReplaceAllString(kiroID, "claude-$1-$2-$3")
}

// anthropicModelItem /v1/models 条目 → Anthropic 形态。
func anthropicModelItem(id string, e map[string]any) map[string]any {
	name, _ := e["name"].(string)
	if name == "" {
		name = id
	}
	created := int64(1753600000)
	if v, ok := usageInt(e["created"]); ok && v > 0 {
		created = int64(v)
	}
	item := map[string]any{
		"type":         "model",
		"id":           id,
		"display_name": name,
		"created_at":   time.Unix(created, 0).UTC().Format(time.RFC3339),
	}
	if v, ok := usageInt(e["context_length"]); ok && v > 0 {
		item["max_input_tokens"] = v
	}
	if v, ok := usageInt(e["max_output_tokens"]); ok && v > 0 {
		item["max_tokens"] = v
	}
	return item
}

// anthropicModelItems Anthropic 形态的模型列表：Kiro 模型排前面、用裸名（/v1/messages 会把裸名
// 路由到 Kiro），其它平台保留前缀。
func (h *Handler) anthropicModelItems() []map[string]any {
	var kiroItems, rest []map[string]any
	seen := map[string]bool{}
	for _, e := range h.modelList() {
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		isKiro := strings.HasPrefix(id, auth.RealmKiro+":")
		if isKiro {
			id = anthropicModelID(strings.TrimPrefix(id, auth.RealmKiro+":"))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if isKiro {
			kiroItems = append(kiroItems, anthropicModelItem(id, e))
		} else {
			rest = append(rest, anthropicModelItem(id, e))
		}
	}
	return append(kiroItems, rest...)
}

// anthropicModels GET /v1/models（Anthropic 形态，支持 after_id / before_id / limit）。
func (h *Handler) anthropicModels(w http.ResponseWriter, r *http.Request) {
	items := h.anthropicModelItems()
	q := r.URL.Query()
	indexOf := func(id string) int {
		for i, it := range items {
			if it["id"] == id {
				return i
			}
		}
		return -1
	}
	if a := q.Get("after_id"); a != "" {
		if i := indexOf(a); i >= 0 {
			items = items[i+1:]
		}
	} else if b := q.Get("before_id"); b != "" {
		if i := indexOf(b); i >= 0 {
			items = items[:i]
		}
	}
	hasMore := false
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n < len(items) {
		if q.Get("before_id") != "" {
			items = items[len(items)-n:]
		} else {
			items = items[:n]
		}
		hasMore = true
	}
	resp := map[string]any{"data": items, "has_more": hasMore, "first_id": nil, "last_id": nil}
	if len(items) > 0 {
		resp["first_id"] = items[0]["id"]
		resp["last_id"] = items[len(items)-1]["id"]
	}
	writeJSON(w, http.StatusOK, resp)
}

// modelByID GET /v1/models/{id}（Anthropic 客户端给 Anthropic 形态，其余给 OpenAI 形态）。
func (h *Handler) modelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if isAnthropicClient(r) {
		items := h.anthropicModelItems()
		for _, it := range items {
			if it["id"] == id {
				writeJSON(w, http.StatusOK, it)
				return
			}
		}
		// 别的写法（点号版本、日期快照、[1m] 后缀）：按 /v1/messages 的路由规则找。
		routed := h.anthropicRouteModel(id)
		if bare, ok := strings.CutPrefix(routed, auth.RealmKiro+":"); ok && h.cfg.Kiro != nil && h.cfg.Kiro.Client != nil {
			if m, found := h.cfg.Kiro.Client.LookupModel(bare); found {
				want := anthropicModelID(m.ID)
				for _, it := range items {
					if it["id"] == want {
						out := map[string]any{}
						for k, v := range it {
							out[k] = v
						}
						out["id"] = id
						writeJSON(w, http.StatusOK, out)
						return
					}
				}
			}
		}
		for _, it := range items {
			if it["id"] == routed {
				writeJSON(w, http.StatusOK, it)
				return
			}
		}
		writeMessagesError(w, http.StatusNotFound, "not_found_error", "model: "+id)
		return
	}
	for _, e := range h.modelList() {
		if e["id"] == id {
			writeJSON(w, http.StatusOK, e)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model not found: "+id)
}
