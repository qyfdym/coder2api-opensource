package proxycore

// 内核安装：从 GitHub Releases 下载（可加镜像前缀），或者接受上传的文件。
// 内核（mihomo，GPL-3.0）不随网关分发，由用户在面板里自行获取。

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	releaseBase = "https://github.com/MetaCubeX/mihomo/releases"
	// maxDownload 下载 / 上传的压缩包上限；maxBinary 解压后的可执行文件上限（当前版本约 60 MB）。
	maxDownload = 150 << 20
	maxBinary   = 250 << 20
)

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// AssetName 当前平台对应的发布包文件名。x86-64 用 compatible 版（不挑 CPU 指令集）。
func AssetName(goos, goarch, version string) (string, error) {
	var stem, ext string
	switch goos + "/" + goarch {
	case "linux/amd64":
		stem, ext = "linux-amd64-compatible", ".gz"
	case "linux/arm64":
		stem, ext = "linux-arm64", ".gz"
	case "linux/386":
		stem, ext = "linux-386", ".gz"
	case "linux/arm":
		stem, ext = "linux-armv7", ".gz"
	case "darwin/amd64":
		stem, ext = "darwin-amd64-compatible", ".gz"
	case "darwin/arm64":
		stem, ext = "darwin-arm64", ".gz"
	case "windows/amd64":
		stem, ext = "windows-amd64-compatible", ".zip"
	case "windows/arm64":
		stem, ext = "windows-arm64", ".zip"
	case "windows/386":
		stem, ext = "windows-386", ".zip"
	default:
		return "", fmt.Errorf("没有 %s/%s 的现成内核包，请手动下载后上传", goos, goarch)
	}
	return "mihomo-" + stem + "-" + version + ext, nil
}

// NormalizeMirror 校验镜像前缀（例如 https://ghfast.top/）。空 = 不用镜像。
func NormalizeMirror(m string) (string, error) {
	m = strings.TrimSpace(m)
	if m == "" {
		return "", nil
	}
	u, err := url.Parse(m)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("镜像前缀要以 https:// 开头，例如 https://ghfast.top/")
	}
	return strings.TrimRight(m, "/") + "/", nil
}

func withMirror(mirror, u string) string {
	if mirror == "" {
		return u
	}
	return mirror + u
}

// InstallProgress 下载进度与最近一次安装的结果（面板轮询用）。
type InstallProgress struct {
	Running bool   `json:"running"`
	Stage   string `json:"stage,omitempty"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	At      int64  `json:"at,omitempty"`
	// SHA256 安装包（压缩包）的 SHA-256；Verified = 已和 GitHub 公布的值对上。
	SHA256   string `json:"sha256,omitempty"`
	Verified bool   `json:"verified,omitempty"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseDigest GitHub 接口公布的发布包 SHA-256（直连 api.github.com，不经镜像：校验值不能和文件来自同一个来源）。
func releaseDigest(ctx context.Context, hc *http.Client, version, asset string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/MetaCubeX/mihomo/releases/tags/"+url.PathEscape(version), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "coder2api")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return "", err
	}
	for _, a := range rel.Assets {
		if a.Name == asset {
			if d, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && len(d) == 64 {
				return strings.ToLower(d), nil
			}
			return "", errors.New("接口没给出这个文件的 SHA-256")
		}
	}
	return "", errors.New("发布里没有 " + asset)
}

type progressReader struct {
	r    io.Reader
	done *atomic.Int64
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done.Add(int64(n))
	return n, err
}

// download 下载最新版内核压缩包，返回压缩包内容、版本号与文件名。
func download(ctx context.Context, hc *http.Client, mirror string, stage func(string), done, total *atomic.Int64) ([]byte, string, string, error) {
	stage("查询最新版本")
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vb, err := httpGet(vctx, hc, withMirror(mirror, releaseBase+"/latest/download/version.txt"), 1<<10, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("查询最新版本失败：%v", err)
	}
	version := strings.TrimSpace(string(vb))
	if !versionRe.MatchString(version) {
		return nil, "", "", fmt.Errorf("查询最新版本失败：返回的不是版本号（%q）", truncateStr(version, 40))
	}
	asset, err := AssetName(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		return nil, "", "", err
	}
	stage("下载 " + asset)
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Minute)
	defer dcancel()
	data, err := httpGet(dctx, hc, withMirror(mirror, releaseBase+"/download/"+version+"/"+asset), maxDownload, func(n int64) {
		total.Store(n)
		done.Store(0)
	}, done)
	if err != nil {
		return nil, "", "", fmt.Errorf("下载 %s 失败：%v", asset, err)
	}
	return data, version, asset, nil
}

func httpGet(ctx context.Context, hc *http.Client, u string, limit int64, onLen func(int64), done ...*atomic.Int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "coder2api")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if onLen != nil && resp.ContentLength > 0 {
		onLen(resp.ContentLength)
	}
	var r io.Reader = resp.Body
	if len(done) > 0 && done[0] != nil {
		r = progressReader{r: r, done: done[0]}
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("文件超过 %d MB", limit>>20)
	}
	return b, nil
}

// extractBinary 从压缩包（.gz / .zip）或裸可执行文件里取出内核，写到 dest（0755）。
func extractBinary(data []byte, dest string) error {
	var r io.Reader
	switch {
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("解压失败：%v", err)
		}
		defer gz.Close()
		r = gz
	case len(data) >= 4 && string(data[:4]) == "PK\x03\x04":
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return fmt.Errorf("解压失败：%v", err)
		}
		var pick *zip.File
		for _, f := range zr.File {
			name := strings.ToLower(filepath.Base(f.Name))
			if f.FileInfo().IsDir() || !strings.HasPrefix(name, "mihomo") {
				continue
			}
			if pick == nil || strings.HasSuffix(name, ".exe") {
				pick = f
			}
		}
		if pick == nil {
			return errors.New("压缩包里没有 mihomo 可执行文件")
		}
		rc, err := pick.Open()
		if err != nil {
			return fmt.Errorf("解压失败：%v", err)
		}
		defer rc.Close()
		r = rc
	case looksExecutable(data):
		r = bytes.NewReader(data)
	default:
		return errors.New("不认识的文件：请上传 mihomo 的 .gz / .zip 发布包或解压后的可执行文件")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBinary+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("解压失败：%v", err)
	}
	if n > maxBinary {
		_ = os.Remove(dest)
		return fmt.Errorf("解压后超过 %d MB，不像是 mihomo", maxBinary>>20)
	}
	return nil
}

// looksExecutable ELF / PE / Mach-O 文件头。
func looksExecutable(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	switch {
	case string(b[:4]) == "\x7fELF", string(b[:2]) == "MZ":
		return true
	}
	magic := []string{"\xfe\xed\xfa\xce", "\xfe\xed\xfa\xcf", "\xce\xfa\xed\xfe", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe"}
	for _, m := range magic {
		if string(b[:4]) == m {
			return true
		}
	}
	return false
}

var versionOutRe = regexp.MustCompile(`Mihomo\s+Meta\s+(v?[0-9][^\s]*)`)

// binaryVersion 运行 `<bin> -v`，确认是 mihomo 并取出版本号。
func binaryVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-v").CombinedOutput()
	if m := versionOutRe.FindSubmatch(out); m != nil {
		return string(m[1]), nil
	}
	if err != nil {
		return "", fmt.Errorf("运行失败：%v（%s）", err, truncateStr(firstLine(out), 120))
	}
	return "", fmt.Errorf("不是 mihomo（-v 输出：%s）", truncateStr(firstLine(out), 120))
}

func firstLine(b []byte) string {
	s := bufio.NewScanner(bytes.NewReader(b))
	if s.Scan() {
		return strings.TrimSpace(s.Text())
	}
	return ""
}

func truncateStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
