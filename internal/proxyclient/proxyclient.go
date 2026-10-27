// Package proxyclient 按代理 URL 复用 http.Client。
//
// upstream（chat 上游）与 oauth（token 刷新）都要按账号走代理，但两包互相
// 不能引用；这里放一个中立的小包，URL → *http.Client 全局缓存，同一代理
// 连接池复用，URL 变更（改 proxies.json 重启）后自然拿到新 client。
package proxyclient

import (
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var cache sync.Map // proxyURL -> *http.Client

// For 返回走指定代理的 client。代理解析失败等错误在请求时暴露（Transport
// 的 Proxy 函数返回 err），这里不做预校验。
func For(proxyURL string) *http.Client {
	if proxyURL == "" {
		return nil
	}
	if v, ok := cache.Load(proxyURL); ok {
		return v.(*http.Client)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	c := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(mustURL(proxyURL)),
			DialContext:           dialer.DialContext,
			ResponseHeaderTimeout: 300 * time.Second, // 流式长回答不能掐
			MaxIdleConns:          8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}
	actual, _ := cache.LoadOrStore(proxyURL, c)
	return actual.(*http.Client)
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		// http.ProxyURL 拿到非法 URL 时每请求报错；这里兜成占位保证不 panic
		return &url.URL{Scheme: "http", Host: s}
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	return u
}
