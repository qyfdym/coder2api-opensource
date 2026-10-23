// Package egress WorkBuddy / Qoder 出站代理的热切换开关。
//
// 两个平台的 HTTP 客户端在装配期就建好了（连接池要复用），所以不在构造时决定代理，而是把
// http.Transport.Proxy 设成 ProxyFunc(平台)：每个请求发出前现查一次当前设置。面板里打开 / 关闭
// 代理立即生效，不用重启；关闭（默认）时与原来完全一样，跟随环境变量 HTTPS_PROXY。
//
// 本包只是个开关，不知道代理池长什么样：具体选哪个代理由装配期注入的 Resolver 决定
// （见 kiroacct.Service.EgressResolver）。
package egress

import (
	"net/http"
	"net/url"
	"sync/atomic"
)

// 平台名（Resolver 的第一个参数）。
const (
	WorkBuddy = "workbuddy"
	Qoder     = "qoder"
)

// Resolver 返回某个平台这次请求该走的代理：(nil, nil) = 没开代理，跟随环境变量；
// (nil, err) = 开了代理但现在没有可用的出口——请求直接失败，不悄悄改走直连。
type Resolver func(platform string, req *http.Request) (*url.URL, error)

var current atomic.Pointer[Resolver]

// Set 装配期注入（nil = 清除，全部跟随环境变量）。
func Set(r Resolver) {
	if r == nil {
		current.Store(nil)
		return
	}
	current.Store(&r)
}

// ProxyFunc 给 http.Transport.Proxy 用的函数。
func ProxyFunc(platform string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if r := current.Load(); r != nil {
			u, err := (*r)(platform, req)
			if err != nil || u != nil {
				return u, err
			}
		}
		return http.ProxyFromEnvironment(req)
	}
}
