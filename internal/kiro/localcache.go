package kiro

// 读取网关所在机器上的 Kiro IDE 登录缓存（~/.aws/sso/cache）。
//
// Kiro IDE 把登录态存成两类文件：
//   - kiro-auth-token.json：accessToken / refreshToken / expiresAt / authMethod / provider / region，
//     IdC 登录还带 clientIdHash；
//   - <clientIdHash>.json：该 IdC 登录注册的 OIDC 客户端（clientId / clientSecret）。
// 这里按 clientIdHash 精确配对（不靠文件顺序猜），读出后走和手工粘贴 JSON 相同的归一流程。
//
// 只读固定的两类文件，目录与文件名都不接受外部输入（hash 严格校验，防路径穿越）。
// 注意：网关刷新 token 会让 IDE 手里的 refresh token 失效（或反过来），两边抢同一份登录态。
// 所以导入走「access token 还有效就先不刷新」的路径，尽量推迟轮换；界面上也有提示。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	localTokenFile    = "kiro-auth-token.json"
	localCacheMaxSize = 1 << 20
)

var clientHashPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// DefaultLocalCacheDir Kiro IDE 缓存目录（Windows：%USERPROFILE%\.aws\sso\cache；macOS / Linux：~/.aws/sso/cache）。
func DefaultLocalCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("无法确定用户主目录")
	}
	return filepath.Join(home, ".aws", "sso", "cache"), nil
}

func readSmallJSON(path string) (map[string]any, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", filepath.Base(path))
	}
	if st.Size() > localCacheMaxSize {
		return nil, fmt.Errorf("%s 超过 1MB", filepath.Base(path))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 JSON", filepath.Base(path))
	}
	return m, nil
}

// ReadLocalCache 读取 dir 下的 Kiro IDE 登录缓存，返回可导入的条目与被跳过的说明。
func ReadLocalCache(dir string) ([]ImportItem, []string) {
	if strings.TrimSpace(dir) == "" {
		return nil, []string{"缓存目录为空"}
	}
	tokenPath := filepath.Join(dir, localTokenFile)
	rec, err := readSmallJSON(tokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, []string{fmt.Sprintf("没有找到 %s（这台机器上的 Kiro IDE 没有登录过，或网关运行在别的机器 / 容器里）", tokenPath)}
		}
		return nil, []string{"读取 " + localTokenFile + " 失败：" + err.Error()}
	}
	hash, _ := rec["clientIdHash"].(string)
	hash = strings.TrimSpace(hash)
	if hash != "" {
		if !clientHashPattern.MatchString(hash) {
			return nil, []string{"clientIdHash 含有非法字符，已拒绝读取配对文件"}
		}
		client, cerr := readSmallJSON(filepath.Join(dir, hash+".json"))
		switch {
		case cerr != nil:
			return nil, []string{fmt.Sprintf("IdC 登录需要配对的 %s.json（OIDC 客户端），读取失败：%v", hash, cerr)}
		default:
			for _, k := range []string{"clientId", "clientSecret"} {
				if v, ok := client[k].(string); ok && strings.TrimSpace(v) != "" {
					rec[k] = v
				}
			}
		}
	}
	return normalizeRecords([]map[string]any{rec})
}
