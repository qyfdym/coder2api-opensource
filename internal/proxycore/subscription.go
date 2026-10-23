// Package proxycore 机场订阅 + 网关托管的 mihomo 代理内核。
//
// 网关不自己解析 vmess / vless / trojan 这些协议，而是托管一个 mihomo 进程：订阅内容原样交给内核
// （内核能解析 Clash 配置、分享链接和 base64 分享链接），每个被勾选的节点由内核在 127.0.0.1 上开一个
// 带认证的代理口，网关把这些口当作普通的 http 代理放进代理池。
//
// 每个节点口背后是一个 select 分组，成员是「这个节点 + 一个永远连不上的守卫节点」，节点排在前面：
// 节点在时走节点；节点从订阅里消失（改名 / 下线）时分组里只剩守卫，请求直接失败，绝不会悄悄改走直连
// （mihomo 对空分组会自动补一个直连成员，所以必须有守卫）。
//
// 设计参考了 Clash Verge Rev（GPL-3.0）的订阅下载与内核托管方式，代码为独立实现。
package proxycore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultUA 拉订阅时的 User-Agent。机场按它决定返回什么格式：带 clash 返回 Clash 配置，
	// 带 verge / meta / mihomo 时多数机场会给出 mihomo 才支持的协议（hysteria2、vless reality 等）。
	DefaultUA = "clash-verge/v2.4.2"
	// fetchTimeout 拉一次订阅的上限。直连失败后还要经代理再试最多 2 次，总耗时要压在
	// 反向代理（Nginx 缺省 60 秒）的超时以内。
	fetchTimeout = 15 * time.Second
	// maxFetchVia 直连失败后最多经几个代理重试。
	maxFetchVia = 2
	// maxSubBytes 订阅内容上限。几百个节点的 Clash 配置也只有几百 KB。
	maxSubBytes = 8 << 20
)

// UserInfo 机场在响应头 subscription-userinfo 里给的流量与到期信息（字节 / Unix 秒，0 = 没给）。
type UserInfo struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
	Expire   int64 `json:"expire"`
}

// FetchResult 一次订阅下载的结果。
type FetchResult struct {
	Body   []byte
	Format string // clash / links / base64
	Info   *UserInfo
	// IntervalMin 机场建议的更新间隔（响应头 profile-update-interval，单位小时，这里换成分钟）；0 = 没给。
	IntervalMin int
	FileName    string
	Home        string
}

// CleanURL 校验订阅地址：只接受 http / https，去掉首尾空白。
func CleanURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("订阅地址是空的")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", errors.New("订阅地址要以 http:// 或 https:// 开头")
	}
	return s, nil
}

// HostOf 订阅地址的主机名（面板里只显示它，完整地址等同于密码）。
func HostOf(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		return u.Hostname()
	}
	return ""
}

// Fetch 用给定的 HTTP 客户端拉一次订阅。非 200、内容太大、内容认不出来都算失败；
// 错误文本里会带上机场返回的错误说明（例如「IP鉴权失败」），但不带订阅地址。
func Fetch(ctx context.Context, hc *http.Client, rawURL, ua string) (FetchResult, error) {
	if strings.TrimSpace(ua) == "" {
		ua = DefaultUA
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return FetchResult{}, errors.New("订阅地址不对")
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	resp, err := hc.Do(req)
	if err != nil {
		return FetchResult{}, scrubURL(err, rawURL)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("读取订阅内容失败：%v", scrubURL(err, rawURL))
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("HTTP %d%s", resp.StatusCode, errorHint(body))
	}
	if len(body) > maxSubBytes {
		return FetchResult{}, fmt.Errorf("订阅内容超过 %d MB", maxSubBytes>>20)
	}
	body = bytes.TrimPrefix(body, []byte("\ufeff"))
	format, err := Sniff(body)
	if err != nil {
		return FetchResult{}, err
	}
	res := FetchResult{Body: body, Format: format, Info: ParseUserInfo(resp.Header)}
	if h, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("profile-update-interval"))); err == nil && h > 0 {
		res.IntervalMin = h * 60
	}
	res.FileName = dispositionName(resp.Header.Get("Content-Disposition"))
	if home := strings.TrimSpace(resp.Header.Get("profile-web-page-url")); home != "" {
		if u, err := url.Parse(home); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			res.Home = home
		}
	}
	return res, nil
}

// errorHint 机场错误响应里的说明文字（JSON 的 error / message / msg 字段，或短文本），截到 160 字。
func errorHint(body []byte) string {
	var m map[string]any
	msg := ""
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"error", "message", "msg", "detail"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				msg = v
				break
			}
		}
	} else if t := strings.TrimSpace(string(body)); t != "" && !strings.Contains(strings.ToLower(t[:min(len(t), 200)]), "<html") {
		msg = t
	}
	if msg == "" {
		return ""
	}
	if r := []rune(msg); len(r) > 160 {
		msg = string(r[:160]) + "…"
	}
	return "：" + msg
}

// scrubURL 去掉错误文本里的订阅地址。net/http 的报错会带上请求的完整 URL，跳转之后是跳转目标
// （同样可能带令牌），所以先剥掉 *url.Error 只留原因，再把原地址替换掉以防万一。
func scrubURL(err error, rawURL string) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		if ue.Timeout() {
			return fmt.Errorf("超时（%s 内没拿到订阅）", fetchTimeout)
		}
		err = ue.Err
	}
	msg := err.Error()
	if rawURL != "" {
		msg = strings.ReplaceAll(msg, rawURL, "<订阅地址>")
		if u, perr := url.Parse(rawURL); perr == nil && u.RawQuery != "" {
			msg = strings.ReplaceAll(msg, u.RawQuery, "***")
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "Client.Timeout") {
		return fmt.Errorf("超时（%s 内没拿到订阅）", fetchTimeout)
	}
	return errors.New(msg)
}

// ParseUserInfo 解析 subscription-userinfo（兼容 x-amz-meta- 之类带前缀的写法）。
func ParseUserInfo(h http.Header) *UserInfo {
	raw := ""
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "subscription-userinfo" || strings.HasSuffix(lk, "-subscription-userinfo") {
			if len(v) > 0 {
				raw = v[0]
				break
			}
		}
	}
	if raw == "" {
		return nil
	}
	info := &UserInfo{}
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || f < 0 {
			continue
		}
		n := int64(f)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload = n
		case "download":
			info.Download = n
		case "total":
			info.Total = n
		case "expire":
			info.Expire = n
		}
	}
	return info
}

// dispositionName Content-Disposition 里的文件名（优先 filename*，去掉 .yaml / .yml 扩展名）。
func dispositionName(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	name := params["filename"] // mime 包已把 filename* 解码进 filename
	name = strings.TrimSpace(name)
	for _, ext := range []string{".yaml", ".yml", ".txt"} {
		name = strings.TrimSuffix(name, ext)
	}
	if r := []rune(name); len(r) > 60 {
		name = string(r[:60])
	}
	return name
}

var (
	clashProxiesRe = regexp.MustCompile(`(?m)^proxies:\s*(#.*)?$`)
	shareLinkRe    = regexp.MustCompile(`(?m)^\s*(ss|ssr|vmess|vless|trojan|hysteria|hysteria2|hy2|tuic|anytls|wireguard|wg|socks|socks5|http|https|snell|mieru)://\S+`)
)

// Sniff 认出订阅内容的格式：Clash 配置（有 proxies 段）、明文分享链接、base64 编码的分享链接。
// 只做识别，真正的解析交给内核。
func Sniff(body []byte) (string, error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return "", errors.New("订阅内容是空的")
	}
	head := strings.ToLower(string(t[:min(len(t), 512)]))
	if strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html") {
		return "", errors.New("机场返回的是网页，不是订阅内容（订阅地址可能填错了，或者需要先登录）")
	}
	if clashProxiesRe.Match(t) {
		return "clash", nil
	}
	if shareLinkRe.Match(t) {
		return "links", nil
	}
	if dec, ok := decodeBase64(t); ok && shareLinkRe.Match(dec) {
		return "base64", nil
	}
	if bytes.Contains(t, []byte("proxy-providers:")) {
		return "", errors.New("这份订阅只引用了别的订阅（proxy-providers），没有节点本身，请换成机场提供的节点订阅地址")
	}
	return "", errors.New("订阅内容认不出来（不是 Clash 配置，也不是分享链接或 base64）")
}

func decodeBase64(b []byte) ([]byte, bool) {
	s := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(b))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := enc.DecodeString(s); err == nil {
			return out, true
		}
	}
	return nil, false
}
