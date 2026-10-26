// oauth 的本地凭据仓库（DPAPI 加密），对应 Python 版 lanes/oauth.py 的
// OAuthCredentialStore。
package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	dir           string // 与 path 同目录；多账号凭据 <account>.bin 都落这
	path          string // 当前激活凭据文件（旧版兼容 = <dir>/oauth-credential.bin）
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

// AccountInfo 多账号列表项（面板展示用；Account 已截断到 12 位）。
type AccountInfo struct {
	Account    string `json:"account"`
	Active     bool   `json:"active"`
	TokenExp   string `json:"token_exp"`
	HasRefresh bool   `json:"has_refresh"`
}

// first12 面板展示统一截断，与启动横幅的口径一致。
func first12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

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
		dir:           directory,
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

// save 落盘（DPAPI 密文，原子写）。多账号：按账号名分文件 `<account>.bin`，
// 并把它记为当前激活账号；account 为空时退回旧版单文件（兼容极端场景）。
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
	path := s.path
	if name := accountFile(cred.Account); name != "" {
		path = filepath.Join(s.dir, name)
	}
	if err := writeFileAtomic(path, blob); err != nil {
		// 落盘失败不影响本次使用，只是下次启动要重新授权
		s.log.Warn("OAuth 凭据落盘失败（下次启动需重新授权）", slog.String("err", err.Error()))
		return
	}
	if path != s.path {
		// 写的是分账号文件：同步激活标记，下次 load/SelectAccount 才能找到它
		_ = writeFileAtomic(filepath.Join(s.dir, ActiveFile), []byte(cred.Account))
	}
}

// loadCurrent 激活账号的凭据文件路径：有 active 标记 → <account>.bin；
// 没有则旧版单文件（升级网关不丢登录态）。
func (s *Store) loadCurrent() string {
	if name := accountFile(readTrim(filepath.Join(s.dir, ActiveFile))); name != "" {
		p := filepath.Join(s.dir, name)
		if fileExists(p) {
			return p
		}
	}
	if fileExists(s.path) {
		return s.path
	}
	return ""
}

func (s *Store) load() *Credential {
	path := s.loadCurrent()
	if path == "" {
		return nil
	}
	blob, err := os.ReadFile(path)
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

// accountFile 账号名 → 凭据文件名。账号名可能含路径分隔符等字符，一律
// sha256 成 hex 再拼 .bin，杜绝目录穿越；空账号返回空串。
func accountFile(account string) string {
	if account == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:16]) + ".bin"
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// SaveLogin 授权成功后入库。多账号：同号授权覆盖旧凭据，异号新开一份
// `<account>.bin` 且立即切换为激活账号——面板上「换个号登录」即换用。
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

// ListAccounts 落盘的全部账号（含激活标记）。旧版单文件迁移前的凭据
// account 为空时显示为 "?"。
func (s *Store) ListAccounts() []AccountInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := readTrim(filepath.Join(s.dir, ActiveFile))
	bins, _ := filepath.Glob(filepath.Join(s.dir, "*.bin"))
	legacy := ""
	if fileExists(s.path) && active == "" {
		legacy = s.path
	}
	var out []AccountInfo
	for _, p := range append(bins, legacy) {
		if p == "" {
			continue
		}
		cred := s.loadFile(p)
		if cred == nil {
			continue
		}
		name := cred.Account
		if name == "" {
			name = "?"
		}
		exp := ""
		if cred.AccessToken != "" {
			exp = FmtExp(cred.AccessToken)
		}
		out = append(out, AccountInfo{
			Account: first12(name), Active: name == active || (legacy != "" && p == legacy),
			TokenExp: exp, HasRefresh: cred.RefreshToken != "",
		})
	}
	return out
}

// SelectAccount 切换激活账号（网关后续请求立刻用新号）。name 必须与
// ListAccounts 返回的 Account 一致（激活标记里存的是完整账号名）。
func (s *Store) SelectAccount(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 从落盘账号里找匹配（列表显示的是前 12 位，全量比对在文件里）
	bins, _ := filepath.Glob(filepath.Join(s.dir, "*.bin"))
	for _, p := range bins {
		cred := s.loadFile(p)
		if cred == nil || cred.Account == "" {
			continue
		}
		if cred.Account == name || first12(cred.Account) == name {
			if err := writeFileAtomic(filepath.Join(s.dir, ActiveFile), []byte(cred.Account)); err != nil {
				return err
			}
			s.cred, s.at = cred, time.Now()
			return nil
		}
	}
	return newErr("没有这个账号的凭据: "+name, nil, nil)
}

// DeleteAccount 删除一个落盘账号的凭据文件；删的是当前激活账号则连
// active 标记一起清掉（下次 Get 报未登录，符合预期）。旧版单文件不在此列。
func (s *Store) DeleteAccount(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bins, _ := filepath.Glob(filepath.Join(s.dir, "*.bin"))
	for _, p := range bins {
		cred := s.loadFile(p)
		if cred == nil {
			continue
		}
		if cred.Account == name || (cred.Account != "" && first12(cred.Account) == name) {
			if err := os.Remove(p); err != nil {
				return err
			}
			if s.cred != nil && s.cred.Account == cred.Account {
				s.cred, s.at = nil, time.Time{}
				_ = os.Remove(filepath.Join(s.dir, ActiveFile))
			}
			return nil
		}
	}
	return newErr("没有这个账号的凭据: "+name, nil, nil)
}

// loadFile 读指定路径的凭据（不碰缓存）。解不开/不合法都返回 nil。
func (s *Store) loadFile(path string) *Credential {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	plain, err := s.unprotectPayload(blob)
	if err != nil {
		return nil
	}
	var data struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		Account      string  `json:"account"`
		ExpiresAt    *int64  `json:"expiresAt"`
		SavedAt      float64 `json:"savedAt"`
	}
	if json.Unmarshal(plain, &data) != nil || data.AccessToken == "" {
		return nil
	}
	return &Credential{
		AccessToken: data.AccessToken, RefreshToken: data.RefreshToken,
		Account: data.Account, ExpiresAt: data.ExpiresAt, SavedAt: data.SavedAt,
	}
}

// Get 返回可用的 accessToken，必要时先刷新。
// Peek 只读缓存的凭据（不触发刷新/落盘），给启动横幅展示账号名用。
// 未登录返回 not ok，调用方自己降级显示 "?"。
func (s *Store) Peek() (*Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred == nil {
		s.cred = s.load()
	}
	return s.cred, s.cred != nil
}

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
			"尚未登录。打开 http://127.0.0.1:%d/admin 用 OAuth 授权登录", s.Port), nil, nil)
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

// ForceRefresh 无视到期时间，立刻用 refreshToken 换新 token 并落盘。
// token 已被上游作废但 refresh 仍有效时（外部脚本重登后旧 access 被轮换），
// 这是免重新授权的恢复路径。
func (s *Store) ForceRefresh() (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred == nil {
		s.cred = s.load()
	}
	cred := s.cred
	if cred == nil {
		return "", newErr("尚未登录，无可刷新的凭据", nil, nil)
	}
	if cred.RefreshToken == "" {
		return "", newErr("这份凭据没有 refreshToken，无法刷新，需重新授权", nil, nil)
	}
	fresh, err := s.httpRefresh(cred.RefreshToken, cred.Account)
	if err != nil {
		return "", err
	}
	s.cred, s.at = fresh, now
	s.save(fresh)
	return fresh.AccessToken, nil
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

// SetRefresher 注入刷新实现（测试用）。
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
func TokenFmtExp(token string) string { return FmtExp(token) }
