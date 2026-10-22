package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// signVectors 由原版 Python 实现（qoder2api-hub 的 qoder_sign / qoder_fingerprint）
// 生成的确定性向量：本包的签名、编码、指纹必须与原版逐字节一致，否则服务端解不开。
type signVectors struct {
	DeriveID      [][3]string            `json:"derive_id"`
	MachineType   [][2]string            `json:"machine_type"`
	MachineToken  [][2]string            `json:"machine_token"`
	Hostname      [][2]string            `json:"hostname"`
	Encode        [][2]string            `json:"encode"` // [std base64 明文, 期望编码]
	SortedCompact []json.RawMessage      `json:"sorted_compact"`
	AESCBC        [][3]string            `json:"aes_cbc"` // [明文 b64, key, 期望密文 b64]
	MD5Sig        [2]string              `json:"md5_sig"`
	RSANBits      int                    `json:"rsa_n_bits"`
	RSAE          int                    `json:"rsa_e"`
	Extra         map[string]interface{} `json:"-"`
}

func loadVectors(t *testing.T) signVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/sign_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v signVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.DeriveID) == 0 || len(v.Encode) == 0 {
		t.Fatal("测试向量为空：用 .wb-test/qd_vectors.py 重新生成")
	}
	return v
}

func TestFingerprintMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.DeriveID {
		if got := DeriveID(c[0], c[1]); got != c[2] {
			t.Errorf("DeriveID(%q,%q)=%q want %q", c[0], c[1], got, c[2])
		}
	}
	for _, c := range v.MachineType {
		if got := MachineType(c[0]); got != c[1] {
			t.Errorf("MachineType(%q)=%q want %q", c[0], got, c[1])
		}
	}
	for _, c := range v.MachineToken {
		if got := MachineToken(c[0]); got != c[1] {
			t.Errorf("MachineToken(%q)=%q want %q", c[0], got, c[1])
		}
	}
	for _, c := range v.Hostname {
		if got := MachineHostname(c[0]); got != c[1] {
			t.Errorf("MachineHostname(%q)=%q want %q", c[0], got, c[1])
		}
	}
}

func TestEncodeMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Encode {
		plain, err := base64.StdEncoding.DecodeString(c[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := Encode(plain); got != c[1] {
			t.Errorf("Encode(%q)=%q want %q", plain, got, c[1])
		}
		back, err := Decode(c[1])
		if err != nil || string(back) != string(plain) {
			t.Errorf("Decode round-trip failed for %q: %v", plain, err)
		}
	}
}

func TestSortedCompactMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, raw := range v.SortedCompact {
		var pair []json.RawMessage
		if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
			t.Fatalf("bad vector: %s", raw)
		}
		var in map[string]string
		var want string
		if err := json.Unmarshal(pair[0], &in); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(pair[1], &want); err != nil {
			t.Fatal(err)
		}
		if got := string(sortedCompactJSON(in)); got != want {
			t.Errorf("sortedCompactJSON mismatch\n got: %s\nwant: %s", got, want)
		}
	}
}

func TestAESCBCMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.AESCBC {
		plain, _ := base64.StdEncoding.DecodeString(c[0])
		got, err := aesCBCEncrypt(plain, []byte(c[1]), []byte(c[1]))
		if err != nil {
			t.Fatal(err)
		}
		if base64.StdEncoding.EncodeToString(got) != c[2] {
			t.Errorf("aesCBCEncrypt(%q) mismatch", plain)
		}
	}
}

func TestSignatureAndKey(t *testing.T) {
	v := loadVectors(t)
	parts := strings.Split(v.MD5Sig[0], "\n")
	if len(parts) != 5 {
		t.Fatalf("bad md5 vector")
	}
	if got := cosySignature(parts[0], parts[1], parts[2], parts[3], parts[4]); got != v.MD5Sig[1] {
		t.Errorf("cosySignature=%s want %s", got, v.MD5Sig[1])
	}
	if serverPub.N.BitLen() != v.RSANBits || serverPub.E != v.RSAE {
		t.Errorf("server key n=%d e=%d want n=%d e=%d", serverPub.N.BitLen(), serverPub.E, v.RSANBits, v.RSAE)
	}
}

// TestSessionInfoDecryptsToIdentity info 必须能用 tempKey 解回 9 键身份体（服务端就是这么解的）。
func TestSessionInfoDecryptsToIdentity(t *testing.T) {
	s, err := NewSession(Cred{UID: "u1", Nickname: "nick", AccessToken: "dt-abc", RefreshToken: "drt-x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.tempKey) != 16 || s.tempKey[12] != '4' {
		t.Errorf("tempKey 形态不对：%q", s.tempKey)
	}
	enc, _ := base64.StdEncoding.DecodeString(s.info)
	blk, _ := aes.NewCipher([]byte(s.tempKey))
	out := make([]byte, len(enc))
	cipher.NewCBCDecrypter(blk, []byte(s.tempKey)).CryptBlocks(out, enc)
	out = out[:len(out)-int(out[len(out)-1])]
	var id map[string]string
	if err := json.Unmarshal(out, &id); err != nil {
		t.Fatalf("info 解不开: %v (%q)", err, out)
	}
	if id["uid"] != "u1" || id["security_oauth_token"] != "dt-abc" || id["user_type"] != DefaultUserType || len(id) != 9 {
		t.Errorf("identity=%v", id)
	}
}

func TestBearerShapeAndPath(t *testing.T) {
	s, err := NewSession(Cred{UID: "u1", AccessToken: "dt-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if p := signPath("https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?x=1"); p != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Errorf("signPath=%q", p)
	}
	_, auth := s.Bearer("{}", "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1")
	if !strings.HasPrefix(auth, "Bearer COSY.") || strings.Count(auth, ".") != 2 {
		t.Errorf("bearer 形态不对：%s", auth)
	}
	h := s.Headers("{}", "https://x/algo/p", HeaderOpts{ModelKey: "qfmodel", SSE: true})
	for _, k := range []string{"cosy-machineid", "cosy-key", "authorization", "x-model-key", "cosy-machinehostname"} {
		if len(h[k]) != 1 || h[k][0] == "" {
			t.Errorf("缺少头 %s", k)
		}
	}
}

func TestSessionCacheRebuildsOnTokenChange(t *testing.T) {
	var c SessionCache
	a, _ := c.Get(Cred{UID: "u", AccessToken: "t1"})
	b, _ := c.Get(Cred{UID: "u", AccessToken: "t1"})
	if a != b {
		t.Error("同 token 应复用会话")
	}
	d, _ := c.Get(Cred{UID: "u", AccessToken: "t2"})
	if d == a {
		t.Error("token 变化应重建会话")
	}
}
