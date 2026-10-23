package proxycore

// 内核控制接口（external-controller）的客户端：只用 /version、/configs、/providers/proxies 这几个稳定接口。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type apiClient struct {
	base   string
	secret string
	hc     *http.Client
}

func (a *apiClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("内核接口 %s %s：HTTP %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, truncateStr(e.Message, 200))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (a *apiClient) version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := a.do(ctx, http.MethodGet, "/version", nil, &v)
	return v.Version, err
}

// reload 让内核重新读取配置文件（path 必须是绝对路径）。
func (a *apiClient) reload(ctx context.Context, absPath string) error {
	return a.do(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": absPath}, nil)
}

// Node 订阅里的一个节点。
type Node struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Delay 最近一次健康检查的延迟（毫秒）；0 = 没测过或不通。
	Delay int `json:"delay"`
	// Tested 测过（Delay 为 0 表示不通）。
	Tested bool `json:"tested"`
}

func (a *apiClient) providerNodes(ctx context.Context, provider string) ([]Node, error) {
	var p struct {
		Proxies []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			History []struct {
				Delay int `json:"delay"`
			} `json:"history"`
		} `json:"proxies"`
	}
	if err := a.do(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(provider), nil, &p); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(p.Proxies))
	for _, x := range p.Proxies {
		n := Node{Name: x.Name, Type: x.Type}
		if h := len(x.History); h > 0 {
			n.Delay, n.Tested = x.History[h-1].Delay, true
		}
		out = append(out, n)
	}
	return out, nil
}

// nodeDelay 让内核测一次节点延迟（经节点访问 healthURL）。不通返回 0。
func (a *apiClient) nodeDelay(ctx context.Context, provider, node string, timeoutMS int) (int, error) {
	var d struct {
		Delay int `json:"delay"`
	}
	path := "/providers/proxies/" + url.PathEscape(provider) + "/" + url.PathEscape(node) +
		"/healthcheck?url=" + url.QueryEscape(healthURL) + "&timeout=" + strconv.Itoa(timeoutMS)
	err := a.do(ctx, http.MethodGet, path, nil, &d)
	return d.Delay, err
}
