// oauth 的本地凭据仓库（DPAPI 加密），对应 Python 版 lanes/oauth.py 的
// OAuthCredentialStore。
package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"autoclaw2api/internal/lanes"
	"autoclaw2api/internal/winapi"
)

// Store 把 OAuth 拿到的 token 用 Windows DPAPI 加密后存文件。
//
// 与桌面端同一套机制：密文绑死当前 Windows 用户与机器，别人拷走文件在别的
// 机器上解不开。Get 到期前 5 分钟就自动刷新，调用方拿到的永远是可用的
// accessToken。
type Store struct {
	Directory string
	Version   string
	Platform  string
	AppID     string
	AppKey    string
	TTL       time.Duration
	Port      int // 仅用于「尚未登录」提示里的面板地址
	AuthBase  string

	Client        *Client
	CallbackPorts []int
	DeviceID      string
	path          string
	log           *slog.Logger
	protect       func([]byte, string) ([]byte, error)
	unprotect     func([]byte) ([]byte, error)
	httpRefresh   func(refreshToken, account string) (*Credential, error)

	mu   sync.Mutex
	cred *Credential
	at   time.Time
}

// RefreshMarginS 提前多久触发刷新。上游 token 有效期以 JWT exp 为准，这里只是
// 兜底窗口。
const RefreshMarginS = 300

// NewStore 构造凭据仓库。device_id 与凭据同目录、同等级保护：它参与签名请求，
// 且必须跨进程稳定。
func NewStore(directory, authBase, version, platform, appID, appKey string,
	extraHosts []string, port int, logger *slog.Logger) *Store {
	deviceID := LoadDeviceID(directory, logger)
	s := &Store{
		Directory: directory, Version: version, Platform: platform,
		AppID: appID, AppKey: appKey, TTL: time.Hour, Port: port,
		AuthBase:      authBase,
		CallbackPorts: CallbackPorts,
		Client:        NewClient(authBase, version, platform, appID, appKey, deviceID, extraHosts),
		DeviceID:      deviceID,
		path:          filepath.Join(directory, CredentialFile),
		log:           logger,
		protect:       winapi.CryptProtectData,
		unprotect:     winapi.CryptUnprotectData,
	}
	s.httpRefresh = s.Client.Refresh
	return s
}

// protectPayload DPAPI 加密。仅 Windows；别的平台直接报错，不静默降级成明文。
func (s *Store) protectPayload(plaintext []byte) ([]byte, error) {
	if s.protect == nil {
		return nil, errors.New("DPAPI 不可用（非 Windows 平台）")
	}
	blob, err := s.protect(plaintext, "AutoClaw2api oauth")
	if err != nil {
		return nil, err
	}
	return blob, nil
}

func (s *Store) unprotectPayload(blob []byte) ([]byte, error) {
	if s.unprotect == nil {
		return nil, errors.New("DPAPI 不可用（非 Windows 平台）")
	}
	return s.unprotect(blob)
}

// save 落盘（DPAPI 密文，原子写）。
func (s *Store) save(cred *Credential) {
	payload, err := json.Marshal(map[string]any{
		"accessToken":  cred.AccessToken,
		"refreshToken": cred.RefreshToken,
		"account":      cred.Account,
		"expiresAt":    cred.ExpiresAt,
		"savedAt":      time.Now().Unix(),
	})
	if err != nil {
		s.log.Warn("OAuth 凭据序列化失败", slog.String("err", err.Error()))
		return
	}
	blob, err := s.protectPayload(payload)
	if err != nil {
		s.log.Warn("OAuth 凭据加密失败（下次启动需重新授权）", slog.String("err", err.Error()))
		return
	}
	if err := writeFileAtomic(s.path, blob); err != nil {
		// 落盘失败不影响本次使用，只是下次启动要重新授权
		s.log.Warn("OAuth 凭据落盘失败（下次启动需重新授权）", slog.String("err", err.Error()))
	}
}

func (s *Store) load() *Credential {
	blob, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	plain, err := s.unprotectPayload(blob)
	if err != nil {
		s.log.Warn("OAuth 凭据解密失败（换用户/换机器？）需重新授权",
			slog.String("err", err.Error()))
		return nil
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Account      string `json:"account"`
		ExpiresAt    *int64 `json:"expiresAt"`
		SavedAt      int64  `json:"savedAt"`
	}
	if json.Unmarshal(plain, &data) != nil {
		s.log.Warn("OAuth 凭据解密失败（换用户/换机器？）需重新授权",
			slog.String("err", "凭据文件不是合法 JSON"))
		return nil
	}
	if data.AccessToken == "" {
		return nil
	}
	return &Credential{
		AccessToken: data.AccessToken, RefreshToken: data.RefreshToken,
		Account: data.Account, ExpiresAt: data.ExpiresAt,
		SavedAt: float64(data.SavedAt),
	}
}

// SaveLogin 授权成功后入库。
func (s *Store) SaveLogin(cred *Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *cred
	s.cred = &c
	s.at = time.Now()
	s.save(cred)
}

// HasSaved 磁盘上是否已有可用凭据。
func (s *Store) HasSaved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load() != nil
}

// Get 返回可用的 accessToken，必要时先刷新。
func (s *Store) Get() (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cred == nil {
		s.cred = s.load()
		s.at = now
	}
	cred := s.cred
	if cred == nil {
		return "", newErr(fmt.Sprintf(
			"尚未登录。打开 http://127.0.0.1:%d/admin 用 OAuth 授权登录，"+
				"或把 AUTOCLAW_CRED_SOURCE 改回 desktop 复用桌面端凭据", s.Port), nil, nil)
	}

	if s.expiring(cred, now) {
		if cred.RefreshToken == "" {
			return "", newErr("token 已过期且没有 refreshToken，需重新授权", nil, nil)
		}
		s.log.Info("OAuth token 即将到期，刷新中")
		fresh, err := s.httpRefresh(cred.RefreshToken, cred.Account)
		if err != nil {
			return "", err
		}
		s.cred, s.at = fresh, now
		s.save(fresh)
		s.log.Info("OAuth token 已刷新")
	}
	return s.cred.AccessToken, nil
}

func (s *Store) expiring(cred *Credential, now time.Time) bool {
	if cred.ExpiresAt != nil && *cred.ExpiresAt > 0 {
		return float64(*cred.ExpiresAt)-float64(now.Unix()) < RefreshMarginS
	}
	// 没有 exp 信息就按 TTL 兜底
	return now.Sub(s.at) >= s.TTL
}

// Invalidate 丢掉内存缓存（下次 Get 重新从磁盘解）。
func (s *Store) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = nil
	s.at = time.Time{}
}

// Flow 起一次授权流程。
func (s *Store) Flow(provider string) (*Flow, error) {
	return newFlow(s, provider)
}

// SetHTTPClient 注入 http.Client（测试用 httptest 假账号面，绝不出网）。
func (s *Store) SetHTTPClient(h *http.Client) {
	s.Client.HTTP = h
}

// SetRefresher 注入刷新实现（测试用；desktop 续期也走同一条路）。
func (s *Store) SetRefresher(fn func(refreshToken, account string) (*Credential, error)) {
	s.httpRefresh = fn
}

// SetDPAPI 注入加解密实现（非 Windows 的测试里绕开 winapi，不落明文）。
func (s *Store) SetDPAPI(p func([]byte, string) ([]byte, error),
	u func([]byte) ([]byte, error)) {
	s.protect, s.unprotect = p, u
}

// Cached 当前内存里的凭据（可能为 nil；只读，不解盘、不刷新）。
// 供 /health 与 overview 展示。
func (s *Store) Cached() *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// TokenFmtExp 面板展示用的到期文案（复用 lanes 的桌面端同一份实现）。
func TokenFmtExp(token string) string { return lanes.FmtExp(token) }
