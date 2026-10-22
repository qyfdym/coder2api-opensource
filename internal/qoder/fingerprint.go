package qoder

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// 设备指纹：一律按账号 UID 稳定派生。
//
// 官方客户端「一个进程 = 一台设备」；网关一个进程同时承载很多账号，若照搬进程级取值
// 会把所有账号关联到同一台机器上。所以按账号派生：同一账号永远同一台虚拟设备，
// 不同账号互相独立。形态与官方一致（machineId 是带连字符的 UUIDv4 形状）。

// DeriveID 由 uid + salt 稳定派生 UUIDv4 形状的标识（版本位固定 4、变体位固定 a）。
// 与原实现 derive_id 逐字符一致（见 testdata 向量）。
func DeriveID(uid, salt string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte(salt + ":" + uid))
	h := hex.EncodeToString(sum[:])
	h = h[:12] + "4" + h[13:16] + "a" + h[17:]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// MachineType 18 位去横线的 machine_type。
func MachineType(uid string) string {
	return strings.ReplaceAll(DeriveID(uid, "machinetype"), "-", "")[:18]
}

// MachineToken base64url 外观的 machine_token（43 字符）。
func MachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	s := strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")
	if len(s) > 43 {
		s = s[:43]
	}
	return s
}

// MachineHostname 按账号稳定派生主机名（形如 DESKTOP-1A2B3C4）。
func MachineHostname(uid string) string {
	if uid == "" {
		uid = "anonymous"
	}
	sum := md5.Sum([]byte("hostname:" + uid))
	return "DESKTOP-" + strings.ToUpper(hex.EncodeToString(sum[:])[:7])
}

// RandomHex 返回 n 字节随机数的十六进制串（请求 id 等：每请求独立随机）。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomUUID 返回随机 UUIDv4（带连字符）。
func RandomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
