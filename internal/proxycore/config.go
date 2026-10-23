package proxycore

// 内核配置生成。输出 JSON（YAML 的子集，mihomo 直接接受），不需要 YAML 库，也不用操心转义。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	// guardName 守卫节点：永远连不上，用来让「节点不在了」的分组故障闭合，而不是被 mihomo 补成直连。
	guardName     = "coder2api-guard"
	guardProvider = "coder2api-guard"
	guardFile     = "guard.yaml"
	guardContent  = "proxies:\n  - {name: " + guardName + ", type: socks5, server: 127.0.0.1, port: 1}\n"
	// healthURL 节点测速与订阅健康检查的目标：Kiro 的推理入口（返回 403 也算通）。
	healthURL = "https://q.us-east-1.amazonaws.com/"
)

// Selection 一个被勾选的节点及其本机代理口。
type Selection struct {
	SubID string `json:"sub_id"`
	Node  string `json:"node"`
	Port  int    `json:"port"`
}

// key 选择的稳定短标识（分组 / 监听口命名用，节点名可能含任意字符）。
func (s Selection) key() string {
	sum := sha1.Sum([]byte(s.SubID + "\x00" + s.Node))
	return hex.EncodeToString(sum[:5])
}

// listenUser 每个节点口自己的用户名。端口释放后再分给别的节点时，代理地址会跟着变，旧节点的
// 固定设置（WorkBuddy / Qoder 的「固定」）、连通状态、账号分配都不会被新节点继承。
func listenUser(base string, s Selection) string { return base + "-" + s.key() }

// excludeTypes 不允许作为节点出口的类型：订阅里混进一个 direct 节点的话，这个口就等于直连。
const excludeTypes = "Direct|Reject|RejectDrop|Pass|Dns|Compatible"

type buildInput struct {
	Controller string
	Secret     string
	User, Pass string
	// Subs 有内容文件的订阅 ID（没内容的订阅不生成 provider，它下面的节点口直接只有守卫）。
	Subs       []string
	Selections []Selection
}

func providerName(subID string) string { return "sub-" + subID }

func providerPath(subID string) string { return "./subs/" + subID + ".txt" }

// nodeFilter 只命中这个节点和守卫节点的正则。
// mihomo 的 filter 用反引号分隔多条正则，节点名里的反引号要写成 \x60。
func nodeFilter(node string) string {
	q := strings.ReplaceAll(regexp.QuoteMeta(node), "`", `\x60`)
	return "^(?:" + q + "|" + regexp.QuoteMeta(guardName) + ")$"
}

func buildConfig(in buildInput) ([]byte, error) {
	health := map[string]any{"enable": true, "lazy": true, "url": healthURL, "interval": 600}
	providers := map[string]any{
		guardProvider: map[string]any{"type": "file", "path": "./" + guardFile},
	}
	hasFile := map[string]bool{}
	for _, id := range in.Subs {
		hasFile[id] = true
		providers[providerName(id)] = map[string]any{"type": "file", "path": providerPath(id), "health-check": health}
	}
	groups := []any{}
	listeners := []any{}
	seen := map[string]bool{}
	for _, s := range in.Selections {
		k := s.key()
		if seen[k] || s.Port <= 0 {
			continue
		}
		seen[k] = true
		use := []string{guardProvider}
		if hasFile[s.SubID] {
			use = []string{providerName(s.SubID), guardProvider} // 节点在前：select 缺省选第一个
		}
		groups = append(groups, map[string]any{
			"name": "n-" + k, "type": "select", "use": use, "filter": nodeFilter(s.Node), "exclude-type": excludeTypes,
		})
		listeners = append(listeners, map[string]any{
			"name": "l-" + k, "type": "mixed", "port": s.Port, "listen": "127.0.0.1", "proxy": "n-" + k,
			"users": []any{map[string]string{"username": listenUser(in.User, s), "password": in.Pass}},
		})
	}
	cfg := map[string]any{
		"mode":                "rule",
		"log-level":           "warning",
		"allow-lan":           false,
		"bind-address":        "127.0.0.1",
		"ipv6":                false,
		"find-process-mode":   "off",
		"geo-auto-update":     false,
		"external-controller": in.Controller,
		"secret":              in.Secret,
		"profile":             map[string]any{"store-selected": false, "store-fake-ip": false},
		"proxy-providers":     providers,
		"proxy-groups":        groups,
		"listeners":           listeners,
		"rules":               []string{"MATCH,REJECT"},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
