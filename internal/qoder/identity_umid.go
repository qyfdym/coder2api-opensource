package qoder

// 从官方 npm 包 @qoder-ai/qodercli 提取内嵌的 UMID 组件 runtime-info（Linux / macOS 用）。
//
// 移植自 qoder2api-hub（MIT License, Copyright (c) 2026 qoder2api-hub authors）_install_umid.py。
//
// 背景：Windows 上组件来自已安装的官方桌面客户端；Linux / macOS / Docker 没有它，只能不发机器头，
// 国际版活动因此常常不可见（见 identity.go）。npm 包是纯 JS bundle，其中
// package/bundle/qoder-worker-runtime.mjs 以长 base64 字面量内嵌了多个平台的原生组件。
// 提取流程：读 registry 元数据 → 下载 tarball（按 npm integrity 做 sha512 校验）→ 解出 bundle →
// 扫描长 base64 字面量 → **按魔数识别格式 / 架构**（不按出现顺序硬编码下标，上游结构随时会变）→
// 选出目标平台组件 → 原子写入 destDir/runtime-info 并 chmod 755。
//
// 只在调用方显式调用时执行（网关不自动下载任何二进制）。机器身份是机器级的：提取组件能让
// Linux 拿到**真身份**，但不能让同一台机器上的多个账号各自领到按设备去重的活动。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	umidRegistryURL  = "https://registry.npmjs.org/@qoder-ai/qodercli"
	umidBundleMember = "package/bundle/qoder-worker-runtime.mjs"
	umidBlobMinLen   = 4000 // 长 base64 字面量阈值（字符数）
	umidUserAgent    = "workbuddy2api-panel/umid-installer"
	umidTargetName   = "runtime-info"
	// 同一目标多个候选时优先选体积落在该区间的（实测 480KB–1MB 的都是真 runtime-info；
	// 7.3MB 的 PE 是 sgsdk 一类的其它原生模块，需避开）。
	umidSizeMin    = 400 * 1024
	umidSizeMax    = 2 * 1024 * 1024
	umidMaxTarball = 256 << 20
	umidMaxBundle  = 512 << 20
)

// FetchRuntimeInfo 从官方 npm 包下载并提取 runtime-info 到 destDir，返回可执行文件路径。
//
// goos / goarch 用 Go 口径（linux|darwin，amd64|arm64；也接受 x86_64 / x64 / aarch64）。
// windows 直接报错：官方桌面客户端自带 resources\umid\runtime-info.exe，MachineIdentity 会自动发现。
// 目标文件已存在且魔数 / 架构校验通过时不重复下载（幂等）。下载走 HTTPS_PROXY 等代理环境变量。
// 把 destDir 设为 Options.DataDir/umid 即可被 MachineIdentity 自动发现。
func FetchRuntimeInfo(ctx context.Context, destDir, goos, goarch string) (string, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	hc := &http.Client{Transport: tr, Timeout: 10 * time.Minute}
	return fetchRuntimeInfo(ctx, hc, umidRegistryURL, destDir, goos, goarch)
}

func fetchRuntimeInfo(ctx context.Context, hc *http.Client, registry, destDir, goos, goarch string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(goos))
	if strings.HasPrefix(p, "win") {
		return "", errors.New("Windows 无需提取：官方桌面客户端自带 resources\\umid\\runtime-info.exe，会被自动发现")
	}
	expected := umidComponentFor(p, goarch)
	if expected == "" {
		return "", fmt.Errorf("不支持的平台/架构组合：%s/%s（支持 linux|darwin + amd64|arm64）", goos, goarch)
	}
	if strings.TrimSpace(destDir) == "" {
		return "", errors.New("destDir 不能为空")
	}
	target := filepath.Join(destDir, umidTargetName)
	if umidInstalled(target, expected) {
		return target, nil
	}
	version, tarURL, integrity, err := umidResolve(ctx, hc, registry)
	if err != nil {
		return "", err
	}
	tgz, err := umidGet(ctx, hc, tarURL, umidMaxTarball)
	if err != nil {
		return "", fmt.Errorf("下载 @qoder-ai/qodercli@%s 失败：%w", version, err)
	}
	if ok, checked := umidVerifyIntegrity(tgz, integrity); checked && !ok {
		return "", fmt.Errorf("@qoder-ai/qodercli@%s tarball 的 sha512 校验失败（可能下载损坏或被篡改）", version)
	}
	text, err := umidExtractBundle(tgz)
	if err != nil {
		return "", err
	}
	blob := umidSelect(umidScan(text, umidBlobMinLen), expected)
	if blob == nil {
		return "", fmt.Errorf("@qoder-ai/qodercli@%s 的 bundle 里没有 %s 组件（上游结构可能已变化）", version, expected)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o755); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	_ = os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return target, nil
}

// umidComponentFor 目标平台 / 架构 → 组件标签；不支持返回空串。
func umidComponentFor(goos, goarch string) string {
	m := strings.ToLower(strings.TrimSpace(goarch))
	switch m {
	case "amd64", "x86_64", "x64":
		m = "x86_64"
	case "arm64", "aarch64":
		m = "arm64"
	}
	switch {
	case strings.HasPrefix(goos, "linux"):
		return map[string]string{"x86_64": "elf-x86_64", "arm64": "elf-aarch64"}[m]
	case goos == "darwin":
		return map[string]string{"x86_64": "macho-x86_64", "arm64": "macho-arm64"}[m]
	}
	return ""
}

// umidIdentify 按魔数识别 blob 的格式与架构：elf-x86_64 / elf-aarch64 / macho-x86_64 /
// macho-arm64 / pe-x86_64 / pe-aarch64 / wasm / unknown。
func umidIdentify(b []byte) string {
	switch {
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("\x00asm")):
		return "wasm"
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("\x7fELF")):
		// EI_CLASS=2（64 位）、EI_DATA=1（小端），e_machine 在 offset 18
		if len(b) < 20 || b[4] != 2 || b[5] != 1 {
			return "unknown"
		}
		switch binary.LittleEndian.Uint16(b[18:20]) {
		case 0x3E:
			return "elf-x86_64"
		case 0xB7:
			return "elf-aarch64"
		}
		return "unknown"
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}):
		if len(b) < 8 {
			return "unknown"
		}
		switch binary.LittleEndian.Uint32(b[4:8]) {
		case 0x01000007:
			return "macho-x86_64"
		case 0x0100000C:
			return "macho-arm64"
		}
		return "unknown"
	case len(b) >= 2 && b[0] == 'M' && b[1] == 'Z':
		if len(b) < 0x40 {
			return "unknown"
		}
		off := int(binary.LittleEndian.Uint32(b[0x3C:0x40]))
		if off < 0 || off+6 > len(b) || !bytes.Equal(b[off:off+4], []byte("PE\x00\x00")) {
			return "unknown"
		}
		switch binary.LittleEndian.Uint16(b[off+4 : off+6]) {
		case 0x8664:
			return "pe-x86_64"
		case 0xAA64:
			return "pe-aarch64"
		}
		return "unknown"
	}
	return "unknown"
}

// umidInstalled 目标文件已存在且校验通过（幂等判定）。
func umidInstalled(path, expected string) bool {
	b, err := os.ReadFile(path)
	return err == nil && umidIdentify(b) == expected
}

// umidResolve 读 npm 元数据 → (version, tarball, integrity)。用精简元数据格式（只含安装所需字段）。
func umidResolve(ctx context.Context, hc *http.Client, registry string) (string, string, string, error) {
	raw, err := umidGetWith(ctx, hc, registry, 64<<20, "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8")
	if err != nil {
		return "", "", "", fmt.Errorf("读取 npm 元数据失败：%w", err)
	}
	meta, ok := oaObject(raw)
	if !ok {
		return "", "", "", errors.New("npm 元数据不是 JSON 对象")
	}
	version := oaStr(oaMap(meta["dist-tags"])["latest"])
	dist := oaMap(oaMap(oaMap(meta["versions"])[version])["dist"])
	tarball := oaStr(dist["tarball"])
	if tarball == "" {
		return "", "", "", fmt.Errorf("npm 元数据里没有 dist.tarball（version=%q）", version)
	}
	return version, tarball, oaStr(dist["integrity"]), nil
}

func umidGet(ctx context.Context, hc *http.Client, u string, limit int64) ([]byte, error) {
	return umidGetWith(ctx, hc, u, limit, "")
}

func umidGetWith(ctx context.Context, hc *http.Client, u string, limit int64, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", umidUserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, oaCut(string(b), 160))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("响应超过 %d 字节上限", limit)
	}
	return b, nil
}

// umidVerifyIntegrity 校验 npm integrity（sha512-<base64>）。checked=false 表示缺失或算法不支持（跳过）。
func umidVerifyIntegrity(data []byte, integrity string) (ok, checked bool) {
	algo, b64, found := strings.Cut(strings.TrimSpace(integrity), "-")
	if !found || algo != "sha512" || b64 == "" {
		return false, false
	}
	want, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false, false
	}
	sum := sha512.Sum512(data)
	return bytes.Equal(sum[:], want), true
}

// umidExtractBundle 从 tarball 取出 worker-runtime bundle 的文本。
func umidExtractBundle(tgz []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return "", fmt.Errorf("tarball 不是 gzip：%w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("解包 tarball 失败：%w", err)
		}
		if strings.TrimLeft(hdr.Name, "./") != umidBundleMember {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, umidMaxBundle))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", fmt.Errorf("tarball 里找不到 %s（上游包结构可能已变化）", umidBundleMember)
}

func umidIsB64(ch byte) bool {
	return (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
		ch == '+' || ch == '/' || ch == '='
}

// umidScan 扫描 bundle 里被同种引号包围、长度 >= minLen 的 base64 字面量并解码
// （等价于原实现正则 (['"])([A-Za-z0-9+/=]{N,})\1；Go 的 regexp 不支持反向引用，故手写）。
// 解码失败的候选跳过；返回顺序即出现顺序（选择逻辑不依赖它）。
func umidScan(text string, minLen int) [][]byte {
	var out [][]byte
	n := len(text)
	for i := 0; i < n; {
		q := text[i]
		if q != '\'' && q != '"' {
			i++
			continue
		}
		j := i + 1
		for j < n && umidIsB64(text[j]) {
			j++
		}
		if j-(i+1) >= minLen && j < n && text[j] == q {
			lit := text[i+1 : j]
			b, err := base64.StdEncoding.DecodeString(lit)
			if err != nil {
				b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(lit, "="))
			}
			if err == nil {
				out = append(out, b)
			}
			i = j + 1
			continue
		}
		// i+1..j-1 都是 base64 字符（不可能是引号），下一个可能的起点就是 j。
		if j > i+1 {
			i = j
		} else {
			i++
		}
	}
	return out
}

// umidSelect 选出 expected 组件：多个匹配时先按体积区间过滤，仍多者取最小。
func umidSelect(cands [][]byte, expected string) []byte {
	var hits [][]byte
	for _, b := range cands {
		if umidIdentify(b) == expected {
			hits = append(hits, b)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	if len(hits) > 1 {
		var sized [][]byte
		for _, b := range hits {
			if len(b) >= umidSizeMin && len(b) <= umidSizeMax {
				sized = append(sized, b)
			}
		}
		if len(sized) > 0 {
			hits = sized
		}
		sort.SliceStable(hits, func(a, b int) bool { return len(hits[a]) < len(hits[b]) })
	}
	return hits[0]
}
