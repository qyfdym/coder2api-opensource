package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Qoder 自定义 Base64 变体（出站请求体编码）
//
//	std        = 标准 Base64(plain)
//	a          = len(std) / 3
//	rearranged = std[n-a:] + std[a:n-a] + std[:a]    （尾 / 中 / 首 三段轮转）
//	逐字符按自定义字母表映射，'=' → '$'
//
// ---------------------------------------------------------------------------
const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var (
	std2custom [256]byte
	custom2std [256]byte
)

func init() {
	for i := 0; i < 64; i++ {
		std2custom[stdAlphabet[i]] = customAlphabet[i]
		custom2std[customAlphabet[i]] = stdAlphabet[i]
	}
	std2custom['='] = customPad
	custom2std[customPad] = '='
}

// Encode 把明文编码成 Qoder 自定义 Base64 变体。
func Encode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, len(rearranged))
	for i := 0; i < len(rearranged); i++ {
		out[i] = std2custom[rearranged[i]]
	}
	return string(out)
}

// Decode Encode 的逆运算（测试与调试用）。
func Decode(encoded string) ([]byte, error) {
	std := make([]byte, len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := custom2std[encoded[i]]
		if c == 0 {
			return nil, fmt.Errorf("qoder decode: invalid char %q at %d", encoded[i], i)
		}
		std[i] = c
	}
	n := len(std)
	a := n / 3
	r1, r2, r3 := std[:a], std[a:n-a], std[n-a:]
	original := make([]byte, 0, n)
	original = append(original, r3...)
	original = append(original, r2...)
	original = append(original, r1...)
	return base64.StdEncoding.DecodeString(string(original))
}

// ---------------------------------------------------------------------------
// 与 Python json.dumps(ensure_ascii=False) 逐字节一致的紧凑排序 JSON
// （服务端签名字节与此强绑定：键升序、无空白、非 ASCII 原样输出）。
// ---------------------------------------------------------------------------

// pyJSONString 按 Python json.dumps(s, ensure_ascii=False) 的规则序列化字符串。
// 与 Go encoding/json 的差异：不转义 < > &、不转义 U+2028/U+2029、控制字符用小写 \u00xx。
func pyJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}

// sortedCompactJSON 键排序 + 无空白的紧凑 JSON（值一律为字符串）。
func sortedCompactJSON(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Python sorted() 对 str 按码点序，与 Go 字节序对 UTF-8 等价
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pyJSONString(k))
		b.WriteByte(':')
		b.WriteString(pyJSONString(m[k]))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// 密码学原语：AES-128-CBC（PKCS7，info 加密用 key == iv）+ RSA PKCS#1 v1.5
// ---------------------------------------------------------------------------

func aesCBCEncrypt(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, buf)
	return out, nil
}

// serverPubPEM Qoder 服务端 RSA-1024 公钥（硬编码自官方客户端，用于包裹会话 AES key）。
// 1024 位 + PKCS#1 v1.5 低于当代推荐强度，但必须与官方私钥配对，属协议层决策，不可单方面更换。
const serverPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var serverPub = func() *rsa.PublicKey {
	blk, _ := pem.Decode([]byte(serverPubPEM))
	if blk == nil {
		panic("qoder: bad server public key PEM")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		panic("qoder: parse server public key: " + err.Error())
	}
	pk, ok := k.(*rsa.PublicKey)
	if !ok {
		panic("qoder: server public key is not RSA")
	}
	return pk
}()

// ---------------------------------------------------------------------------
// COSY 会话与 Bearer 签名
//
//	tempKey  = 16 个十六进制字符（作为 16 字节 AES-128 key/iv；熵 64 bit，属协议口径）
//	cosyKey  = base64(RSA_PKCS1v15(tempKey))
//	info     = base64(AES-CBC(identity 排序紧凑 JSON, key=iv=tempKey))
//	payload  = base64(排序紧凑 JSON {cosyVersion, ideVersion, info, requestId, version})
//	sig      = hex(md5(payload \n cosyKey \n date \n body \n path))，path 去掉 /algo 前缀
//	Bearer   = "Bearer COSY." + payload + "." + sig
//
// ---------------------------------------------------------------------------

// CosyVersion 官方 SDK 内置的 COSY 协议版本（cosy-version 头与 payload 共用）。
var CosyVersion = "1.1.64"

// CosyMachineOS 推理头 cosy-machineos（与官方 infer-sse 一致：按宿主平台）。
var CosyMachineOS = func() string {
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}()

// Session 单账号的 COSY 签名会话。machine 系列按 UID 稳定派生；
// tempKey / cosyKey / info 在创建时生成，access token 轮换后整体重建。
type Session struct {
	UID             string
	MachineID       string
	MachineToken    string
	MachineType     string
	MachineHostname string
	accessToken     string
	tempKey         string
	cosyKey         string
	info            string
}

// NewSession 按凭证构造 COSY 会话。
func NewSession(c Cred) (*Session, error) {
	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, fmt.Errorf("qoder cosy: empty access token")
	}
	s := &Session{
		UID:             c.UID,
		MachineID:       DeriveID(c.UID, "machine"),
		MachineToken:    MachineToken(c.UID),
		MachineType:     MachineType(c.UID),
		MachineHostname: MachineHostname(c.UID),
		accessToken:     c.AccessToken,
		// 16 个十六进制字符，取自随机 uuid4 的 hex 前 16 位（与官方 uuid4().hex[:16] 同形）。
		tempKey: strings.ReplaceAll(RandomUUID(), "-", "")[:16],
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, serverPub, []byte(s.tempKey))
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: rsa: %w", err)
	}
	s.cosyKey = base64.StdEncoding.EncodeToString(wrapped)
	userType := c.UserType
	if userType == "" {
		userType = DefaultUserType
	}
	// 身份体 9 键（增删键必须与服务端预期同步）。
	identity := map[string]string{
		"name":                 c.Nickname,
		"aid":                  c.UID,
		"uid":                  c.UID,
		"yx_uid":               "",
		"organization_id":      c.OrgID,
		"organization_name":    c.OrgName,
		"user_type":            userType,
		"security_oauth_token": c.AccessToken,
		"refresh_token":        c.RefreshToken,
	}
	enc, err := aesCBCEncrypt(sortedCompactJSON(identity), []byte(s.tempKey), []byte(s.tempKey))
	if err != nil {
		return nil, fmt.Errorf("qoder cosy: aes: %w", err)
	}
	s.info = base64.StdEncoding.EncodeToString(enc)
	return s, nil
}

// signPath 取 URL path 并去掉 /algo 前缀（签名覆盖的 path 口径）。
func signPath(rawURL string) string {
	path := "/"
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	return strings.TrimPrefix(path, "/algo")
}

// cosySignature 计算 md5 签名（拆出来便于用固定输入做向量测试）。
func cosySignature(payloadB64, cosyKey, date, body, path string) string {
	sum := md5.Sum([]byte(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + path))
	return hex.EncodeToString(sum[:])
}

// Bearer 返回 (date, authorization)。签名覆盖 body 与去 /algo 的 path。
func (s *Session) Bearer(body, rawURL string) (date, authorization string) {
	payload := map[string]string{
		"cosyVersion": CosyVersion,
		"ideVersion":  "",
		"info":        s.info,
		"requestId":   RandomUUID(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(sortedCompactJSON(payload))
	date = strconv.FormatInt(time.Now().Unix(), 10)
	sig := cosySignature(payloadB64, s.cosyKey, date, body, signPath(rawURL))
	return date, "Bearer COSY." + payloadB64 + "." + sig
}

// HeaderOpts 推理 / 模型接口签名头的可选项。
type HeaderOpts struct {
	ModelKey string // 非空时带 x-model-key（决定上游模型路由）
	SSE      bool   // 推理 SSE：带 cache-control 与 machineos/hostname
	Accept   string // 缺省 text/event-stream
}

// Headers 构造一次推理 / 模型接口的完整 COSY 签名头。
func (s *Session) Headers(body, rawURL string, o HeaderOpts) http.Header {
	date, bearer := s.Bearer(body, rawURL)
	accept := o.Accept
	if accept == "" {
		accept = "text/event-stream"
	}
	h := http.Header{}
	set := func(k, v string) { h[k] = []string{v} } // 保持小写原样（与原实现一致）
	set("cosy-data-policy", "AGREE")
	set("content-type", "application/json")
	set("cosy-machinetype", s.MachineType)
	set("cosy-clienttype", "5")
	set("cosy-date", date)
	set("cosy-user", s.UID)
	set("cosy-key", s.cosyKey)
	set("accept", accept)
	set("cosy-clientip", "169.254.198.161")
	set("authorization", bearer)
	set("accept-encoding", "identity")
	set("cosy-version", CosyVersion)
	set("cosy-machineid", s.MachineID)
	set("cosy-machinetoken", s.MachineToken)
	set("login-version", "v2")
	set("user-agent", "Go-http-client/2.0")
	if o.SSE {
		set("cache-control", "no-cache")
		set("cosy-machineos", CosyMachineOS)
		set("cosy-machinehostname", s.MachineHostname)
	}
	if o.ModelKey != "" {
		set("x-model-key", o.ModelKey)
		set("x-model-source", "system")
	}
	return h
}

// SessionCache 账号 UID → Session 的进程级缓存；access token 变化即重建。
type SessionCache struct {
	mu sync.Mutex
	m  map[string]cachedSession
}

type cachedSession struct {
	token string
	sess  *Session
}

// Get 取（或新建）该凭证的 COSY 会话。
func (c *SessionCache) Get(cred Cred) (*Session, error) {
	key := cred.UID
	if key == "" {
		if len(cred.AccessToken) > 16 {
			key = cred.AccessToken[:16]
		} else {
			key = cred.AccessToken
		}
	}
	c.mu.Lock()
	if hit, ok := c.m[key]; ok && hit.token == cred.AccessToken {
		c.mu.Unlock()
		return hit.sess, nil
	}
	c.mu.Unlock()
	s, err := NewSession(cred)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]cachedSession)
	}
	c.m[key] = cachedSession{token: cred.AccessToken, sess: s}
	c.mu.Unlock()
	return s, nil
}

// Invalidate 丢弃某账号的会话（token 刷新后由调用方触发；Get 也会按 token 自动重建）。
func (c *SessionCache) Invalidate(uid string) {
	c.mu.Lock()
	delete(c.m, uid)
	c.mu.Unlock()
}
