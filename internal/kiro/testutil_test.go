package kiro

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// esHeader 编码一个字符串类型（7）的 event-stream 头。
func esHeader(name, value string) []byte {
	var b bytes.Buffer
	b.WriteByte(byte(len(name)))
	b.WriteString(name)
	b.WriteByte(7)
	_ = binary.Write(&b, binary.BigEndian, uint16(len(value)))
	b.WriteString(value)
	return b.Bytes()
}

// esFrame 编码一帧 AWS event-stream 消息（含两段 CRC）。
func esFrame(headers map[string]string, payload any) []byte {
	var hb bytes.Buffer
	// 固定顺序，保证输出确定。
	for _, k := range []string{":message-type", ":event-type", ":exception-type", ":content-type"} {
		if v, ok := headers[k]; ok {
			hb.Write(esHeader(k, v))
		}
	}
	var body []byte
	switch p := payload.(type) {
	case nil:
	case []byte:
		body = p
	default:
		body, _ = json.Marshal(p)
	}
	total := 12 + hb.Len() + len(body) + 4
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(hb.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[:8]))
	msg := append(hb.Bytes(), body...)
	crc := crc32.Update(crc32.Update(0, crc32.IEEETable, prelude), crc32.IEEETable, msg)
	out := append(prelude, msg...)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc)
	return append(out, tail[:]...)
}

func evt(eventType string, payload any) []byte {
	return esFrame(map[string]string{":message-type": "event", ":event-type": eventType, ":content-type": "application/json"}, payload)
}

func exceptionFrame(exType, message string) []byte {
	return esFrame(map[string]string{":message-type": "exception", ":exception-type": exType}, map[string]string{"message": message})
}

func joinFrames(frames ...[]byte) []byte { return bytes.Join(frames, nil) }

// fakeUpstream 按「主机 + 路径」路由的假上游（经 Options.Transport 注入，不开端口）。
type fakeUpstream struct {
	mu       sync.Mutex
	handlers map[string]func(r *http.Request, body []byte) (int, http.Header, []byte)
	requests []recorded
}

type recorded struct {
	Host   string
	Path   string
	Header http.Header
	Body   []byte
}

func newFake() *fakeUpstream {
	return &fakeUpstream{handlers: map[string]func(*http.Request, []byte) (int, http.Header, []byte){}}
}

func (f *fakeUpstream) on(hostPath string, h func(r *http.Request, body []byte) (int, http.Header, []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[hostPath] = h
}

func (f *fakeUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Host: r.URL.Host, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	h := f.handlers[r.URL.Host+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"no route"}`)), Header: http.Header{}, Request: r}, nil
	}
	status, hdr, out := h(r, body)
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(out)), Header: hdr, Request: r}, nil
}

func (f *fakeUpstream) calls(hostPath string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Host+r.Path == hostPath {
			out = append(out, r)
		}
	}
	return out
}

func testClient(f *fakeUpstream) *Client {
	return NewClient(Options{Transport: f, Logf: func(string, ...any) {}})
}

const (
	hostQ      = "q.us-east-1.amazonaws.com"
	hostCW     = "codewhisperer.us-east-1.amazonaws.com"
	pathGen    = "/generateAssistantResponse"
	routeIDE   = hostQ + pathGen
	routeCW    = hostCW + pathGen
	routeUsage = hostCW + "/getUsageLimits"
)

func streamResp(frames ...[]byte) func(*http.Request, []byte) (int, http.Header, []byte) {
	return func(*http.Request, []byte) (int, http.Header, []byte) {
		return 200, http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}}, joinFrames(frames...)
	}
}

func errResp(status int, body string) func(*http.Request, []byte) (int, http.Header, []byte) {
	return func(*http.Request, []byte) (int, http.Header, []byte) { return status, nil, []byte(body) }
}

// sseFrames 把 SSE 文本拆成 data 帧（不含 [DONE]）。
func sseFrames(t *testing.T, raw string) (frames []map[string]any, done bool) {
	t.Helper()
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(block, "data:"))
		if data == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad SSE frame %q: %v", data, err)
		}
		frames = append(frames, m)
	}
	return frames, done
}

// collect 汇总一组 SSE 帧：正文、思考、工具调用、finish_reason、usage、error。
type collected struct {
	content, reasoning string
	tools              []map[string]any
	finish             string
	usage              map[string]any
	errFrame           map[string]any
	role               bool
}

func collect(frames []map[string]any) collected {
	var c collected
	for _, f := range frames {
		if e, ok := f["error"].(map[string]any); ok {
			c.errFrame = e
			continue
		}
		if u, ok := f["usage"].(map[string]any); ok {
			c.usage = u
		}
		ch, _ := f["choices"].([]any)
		for _, x := range ch {
			m := x.(map[string]any)
			if fr, ok := m["finish_reason"].(string); ok {
				c.finish = fr
			}
			d, _ := m["delta"].(map[string]any)
			if d["role"] == "assistant" {
				c.role = true
			}
			if s, ok := d["content"].(string); ok {
				c.content += s
			}
			if s, ok := d["reasoning_content"].(string); ok {
				c.reasoning += s
			}
			if tcs, ok := d["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					c.tools = append(c.tools, tc.(map[string]any))
				}
			}
		}
	}
	return c
}
