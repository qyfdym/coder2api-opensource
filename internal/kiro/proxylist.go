package kiro

// 代理列表文本的解析：面板「代理池」页一次粘贴多行。

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ProxyItem 从文本里解析出的一条代理。
type ProxyItem struct {
	// URL 规范化后的完整地址（可能带账号密码）。
	URL string
	// Note 地址后面的备注。
	Note string
}

// ParseProxyList 解析多行代理文本，一行一个，空行与 # 开头的行忽略。支持的写法（地址后面空格接备注）：
//
//	http://host:port            socks5://user:pass@host:port    （带协议的完整地址）
//	host:port                   user:pass@host:port             （没写协议 → 用 defaultScheme）
//	host:port:user:pass                                         （常见的代理商导出格式）
//
// defaultScheme 只接受 http / https / socks5 / socks5h，其它值按 http。
// 解析失败的行不影响其它行，原因（带行号）放在 skipped 里；同一次粘贴里重复的地址只保留第一条。
func ParseProxyList(text, defaultScheme string) (items []ProxyItem, skipped []string) {
	scheme := strings.ToLower(strings.TrimSpace(defaultScheme))
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		scheme = "http"
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		raw, err := parseProxyAddr(fields[0], scheme)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("第 %d 行：%s", i+1, err))
			continue
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		items = append(items, ProxyItem{URL: raw, Note: strings.Join(fields[1:], " ")})
	}
	return items, skipped
}

func parseProxyAddr(addr, scheme string) (string, error) {
	if strings.Contains(addr, "://") {
		n, err := NormalizeProxyURL(addr)
		if err != nil {
			return "", err
		}
		if n == "" || strings.EqualFold(n, ProxyDirect) {
			return "", errors.New("不是代理地址")
		}
		return n, nil
	}

	var user, pass, hostport string
	hasUser := false
	switch {
	case strings.Contains(addr, "@"):
		at := strings.LastIndex(addr, "@")
		user, pass, _ = strings.Cut(addr[:at], ":")
		hostport, hasUser = addr[at+1:], true
	case strings.HasPrefix(addr, "["): // [IPv6]:port
		hostport = addr
	default:
		parts := strings.SplitN(addr, ":", 4)
		switch len(parts) {
		case 2:
			hostport = addr
		case 4:
			hostport, user, pass, hasUser = parts[0]+":"+parts[1], parts[2], parts[3], true
		default:
			return "", errors.New("格式不识别，应为 host:port、user:pass@host:port、host:port:user:pass 或完整地址")
		}
	}

	host, port, err := net.SplitHostPort(hostport)
	if err != nil || host == "" {
		return "", errors.New("需要 host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", errors.New("端口不对")
	}
	u := url.URL{Scheme: scheme, Host: hostport}
	if hasUser {
		if user == "" {
			return "", errors.New("用户名为空")
		}
		if pass == "" {
			u.User = url.User(user)
		} else {
			u.User = url.UserPassword(user, pass)
		}
	}
	return NormalizeProxyURL(u.String())
}
