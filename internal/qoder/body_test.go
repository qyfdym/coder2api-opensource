package qoder

import (
	"os"
	"testing"
)

// blankBody 把随机字段清零，与 Python 向量生成器 blank() 对齐。
func blankBody(body *jval) {
	for _, k := range []string{"request_id", "chat_record_id", "request_set_id", "session_id"} {
		if body.has(k) {
			body.set(k, jStrV(""))
		}
	}
	if biz := body.get("business"); biz.isObj() {
		biz.set("id", jStrV(""))
		biz.set("begin_at", jIntV(0))
	}
}

func TestBuildQoderBodyVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/qd_body_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	vecs, err := parseJ(raw)
	if err != nil || !vecs.isArr() {
		t.Fatalf("parse vectors: %v", err)
	}
	c := testClient()
	for _, v := range vecs.arr {
		name := v.get("name").str()
		realm := v.get("realm").str()
		modelKey := v.get("model_key").str()
		payload := v.get("payload")
		expected := v.get("expected")

		cred := Cred{Realm: realm}
		got := c.buildQoderBody(payload, cred, modelKey, realm)
		blankBody(got)

		gotStr := got.dumpPy()
		wantStr := expected.dumpPy()
		if gotStr != wantStr {
			// 定位首个不同的键，便于排障。
			t.Errorf("case %q: body mismatch\nfirst diff: %s", name, firstDiff(gotStr, wantStr))
		}
	}
}

func firstDiff(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			lo := i - 60
			if lo < 0 {
				lo = 0
			}
			hiA := i + 60
			if hiA > len(a) {
				hiA = len(a)
			}
			hiB := i + 60
			if hiB > len(b) {
				hiB = len(b)
			}
			return "at byte " + itoa(i) + "\n got: ..." + a[lo:hiA] + "...\nwant: ..." + b[lo:hiB] + "..."
		}
	}
	if len(a) != len(b) {
		return "length differs: got " + itoa(len(a)) + " want " + itoa(len(b))
	}
	return "(no byte diff?)"
}

func TestEncodeBodyRoundTrip(t *testing.T) {
	// 签名覆盖的 body 与实际发送的 body 必须是同一字符串：Encode 后能 Decode 回原文。
	c := testClient()
	payload, _ := parseJString(`{"model":"qmodel_38max","messages":[{"role":"user","content":"通了"}]}`)
	body := c.buildQoderBody(payload, Cred{Realm: RealmCN}, "qmodel_38max", RealmCN)
	js := body.dumpPy()
	enc := Encode([]byte(js))
	dec, err := Decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != js {
		t.Fatal("encode/decode round-trip mismatch")
	}
}
