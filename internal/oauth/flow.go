// oauth 的授权流程编排，对应 Python 版 lanes/oauth.py 的 OAuthFlow 与
// _CallbackHandler。
package oauth

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// 回调页文案。给中文用户看，所以照 Python 版原样保留。
const (
	pageFailed = `<!doctype html><html lang='zh-CN'><head><meta charset='utf-8'>` +
		`<title>授权未完成</title></head><body style='font:14px system-ui;` +
		`display:grid;place-items:center;min-height:100vh;margin:0'>` +
		`<p>授权没有完成，可以关掉这个页面回 AutoClaw2api 重试。</p></body></html>`
	pageOK = `<!doctype html><html lang='zh-CN'><head><meta charset='utf-8'>` +
		`<title>授权成功</title></head><body style='font:14px system-ui;` +
		`display:grid;place-items:center;min-height:100vh;margin:0'>` +
		`<p>已收到授权，可以关掉这个页面回 AutoClaw2api 了。</p></body></html>`
)

// callbackResult provider 302 回来的 code / state。
type callbackResult struct {
	Code  string
	State string
}

// Flow 一次授权流程的编排：起 loopback → 取授权链接 → 等回调 → 换 token。
//
// Begin 与 Wait 分开，是为了让调用方（HTTP 路由）能先把授权链接返回给面板，
// 再由浏览器真正发起授权。
type Flow struct {
	store    *Store
	provider string
	path     string

	client *Client

	server   *http.Server
	listener net.Listener
	port     int
	url      string

	mu        sync.Mutex
	settled   bool
	cancelled bool
	result    *callbackResult
	closed    bool
}

func newFlow(store *Store, provider string) (*Flow, error) {
	if _, ok := OAuthHosts[provider]; !ok {
		return nil, newErr("provider 只能是 "+providerList(), nil, nil)
	}
	return &Flow{
		store: store, provider: provider, path: CallbackPaths[provider],
		client: store.Client,
	}, nil
}

// Begin 起回调服务并取授权链接，返回给用户打开的 URL。
func (f *Flow) Begin() (string, error) {
	if err := f.bind(); err != nil {
		return "", err
	}
	navigateURI := fmt.Sprintf("http://localhost:%d%s", f.port, f.path)
	u, err := f.client.OAuthURL(f.provider, navigateURI)
	if err != nil {
		f.close()
		return "", err
	}
	f.url = u
	return u, nil
}

// URL 授权链接（Begin 之后有效）。
func (f *Flow) URL() string { return f.url }

// Port loopback 实际占用的端口。
func (f *Flow) Port() int { return f.port }

// Wait 阻塞等回调，拿到 code/state 后立刻换 token 并落盘。
func (f *Flow) Wait(timeout time.Duration) (*Credential, error) {
	if f.server == nil {
		return nil, newErr("先调 Begin() 再 Wait()", nil, nil)
	}
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		settled, cancelled, result := f.settled, f.cancelled, f.result
		f.mu.Unlock()
		if cancelled {
			// Python 版这里会让 wait() 摸到已经 shutdown 并置 None 的
			// self._server，抛 AttributeError 再被 worker 兜住——症状是面板
			// 显示一串 'NoneType' object has no attribute 'settled'。
			// Go 版给一句人话，行为语义（取消即失败结束）不变。
			return nil, newErr("授权已取消", nil, nil)
		}
		if settled {
			if result == nil {
				return nil, newErr("授权没有完成", nil, nil)
			}
			cred, err := f.client.ExchangeOAuth(
				f.provider, result.Code, result.State,
				fmt.Sprintf("http://localhost:%d%s", f.port, f.path))
			if err != nil {
				return nil, err
			}
			f.store.SaveLogin(cred)
			return cred, nil
		}
		if time.Now().After(deadline) {
			return nil, newErr(fmt.Sprintf("等授权等超时了（%d 秒），重新走一遍",
				int(timeout.Seconds())), nil, nil)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// Cancel 放弃授权：关掉 loopback，让等待中的线程退出。
func (f *Flow) Cancel() {
	f.mu.Lock()
	f.settled = true
	f.mu.Unlock()
	f.close()
}

// bind 按 main.cjs 的端口顺序试，全占用才失败。
func (f *Flow) bind() error {
	var lastErr error
	for _, port := range f.store.CallbackPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			lastErr = err
			continue
		}
		f.listener, f.port = ln, port
		srv := &http.Server{Handler: f.handler()}
		f.server = srv
		go func() {
			// Serve 的返回错误一律忽略：正常结束时是 ErrServerClosed，
			// 异常时结果与超时路径同样由 Wait 报给调用方
			_ = srv.Serve(ln)
		}()
		return nil
	}
	return newErr(fmt.Sprintf("回调端口全被占用了（%v），关掉占用进程再试: %v",
		f.store.CallbackPorts, lastErr), nil, nil)
}

func (f *Flow) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	srv, ln := f.server, f.listener
	f.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// handler 接 provider 302 回来的 code / state，然后回一页给人看。
//
// 与 main.cjs 的判定一致：路径不对 404 页，缺 code 或缺 state 算取消
// （服务端可能带 error=access_denied 跳回来）。
func (f *Flow) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.ParseRequestURI(r.RequestURI)
		var query url.Values
		if u != nil {
			query = u.Query()
		}
		code := query.Get("code")
		state := query.Get("state")
		denied := query.Has("error") || code == "" || state == ""
		ok := u != nil && u.Path == f.path && !denied

		page := []byte(pageFailed)
		status := http.StatusBadRequest
		if ok {
			page = []byte(pageOK)
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(page)))
		w.WriteHeader(status)
		_, _ = w.Write(page)

		if ok {
			f.mu.Lock()
			if !f.settled {
				f.settled = true
				f.result = &callbackResult{Code: code, State: state}
			}
			f.mu.Unlock()
		}
	})
}
