// Package lanes 是 AutoClaw 上游通道层，对应 Python 版 lanes/autoclaw.py：
// 凭据解密（Chromium os_crypt：DPAPI 护主密钥 + AES-256-GCM 护业务文件）、
// 请求整形（406 三条硬约束）、SSE/JSON 解析、模型目录与上游错误归一。
//
// 三条硬约束来自本机实测（docs/autoclaw-10-2x门禁与反代落地.md）：
//  1. 请求体绝不能带 max_tokens / max_completion_tokens —— 否则 406 空 body
//  2. system 必须含 2.x banner —— 否则 406 空 body
//  3. 绝不发 X-Harness-Type —— 2.x 宿主上它 = 406 空 body
package lanes

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"autoclaw2api/internal/winapi"
)

// Credential 是解出的桌面端凭据（字段与 Python decrypt_credential() 返回一致）。
type Credential struct {
	AccessToken  string
	RefreshToken string
	UpdatedAt    string // 原样透传（ISO8601 字符串）
	Account      string // accounts/ 下的目录名（sha256(accountId)）
	Path         string
}

const tokenTTL = 5 * time.Minute // 凭据文件最多 5 分钟重读一次

// ── os_crypt 解密 ────────────────────────────────────────────────────────

// loadMasterKey: Local State → os_crypt.encrypted_key → DPAPI → 32B AES-256 主密钥。
func loadMasterKey(base string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(base, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败: %w", err)
	}
	var state struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("Local State 解析失败: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(state.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key base64 解析失败: %w", err)
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, errors.New("Local State 的 encrypted_key 不是 DPAPI 前缀，格式不符")
	}
	key, err := winapi.CryptUnprotectData(blob[5:])
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥长度 %d，预期 32", len(key))
	}
	return key, nil
}

// decryptV10: "v10" || nonce(12) || ciphertext || tag(16) → plaintext。
func decryptV10(key, blob []byte) ([]byte, error) {
	if len(blob) < 3 || string(blob[:3]) != "v10" {
		return nil, fmt.Errorf("凭据文件不是 v10 格式（magic=%q）", blob[:3])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, sealed := blob[3:15], blob[15:]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-256-GCM 解密失败（换用户/换机器？）: %w", err)
	}
	return plain, nil
}

// DiscoverAccount 返回 accounts/ 下最近修改的账号目录名（sha256(accountId)）。
// 桌面端同时登录多号的场景极少，按 mtime 选至少是确定性的。
func DiscoverAccount(base string) (string, error) {
	root := filepath.Join(base, "accounts")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("找不到 AutoClaw 账号目录: %s", root)
	}
	type dirInfo struct {
		name  string
		mtime time.Time
	}
	var dirs []dirInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, dirInfo{name: e.Name(), mtime: info.ModTime()})
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("AutoClaw 账号目录为空: %s", root)
	}
	best := dirs[0]
	for _, d := range dirs[1:] {
		if d.mtime.After(best.mtime) {
			best = d
		}
	}
	return best.name, nil
}

// DecryptCredential 解出桌面端凭据。只读不写——桌面端刷新登录态后按 mtime 自动跟随。
func DecryptCredential(base, account string) (*Credential, error) {
	if account == "" {
		var err error
		if account, err = DiscoverAccount(base); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(base, "accounts", account, "account-credentials.enc")
	key, err := loadMasterKey(base)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败: %w", err)
	}
	plain, err := decryptV10(key, blob)
	if err != nil {
		return nil, err
	}
	var cred struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		UpdatedAt    any    `json:"updatedAt"`
	}
	if err := json.Unmarshal(plain, &cred); err != nil {
		return nil, fmt.Errorf("凭据 JSON 解析失败: %w", err)
	}
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		return nil, errors.New("凭据文件里没有 accessToken（可能已退出登录）")
	}
	rt := strings.TrimSpace(cred.RefreshToken)
	up, _ := cred.UpdatedAt.(string)
	return &Credential{
		AccessToken:  token,
		RefreshToken: rt,
		UpdatedAt:    up,
		Account:      account,
		Path:         path,
	}, nil
}

// TokenExpUnix 从 JWT 读 exp（秒）；解不出返回 0。只用于日志/探活，不参与决策。
func TokenExpUnix(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
			return 0
		}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	n, err := claims.Exp.Int64()
	if err != nil || n == 0 {
		return 0
	}
	return n
}

// FmtExp 对应 Python _fmt_exp："2026-01-02 15:04:05 (16.2h)"；解不出返回 "?"。
func FmtExp(token string) string {
	exp := TokenExpUnix(token)
	if exp == 0 {
		return "?"
	}
	left := float64(exp-time.Now().Unix()) / 3600.0
	return fmt.Sprintf("%s (%.1fh)", time.Unix(exp, 0).Format("2006-01-02 15:04:05"), left)
}

// CredentialStore 线程安全的凭据缓存：按 mtime 热重载，桌面端刷新登录态后自动跟随。
type CredentialStore struct {
	base    string
	account string
	ttl     time.Duration

	mu    sync.Mutex
	cred  *Credential
	at    time.Time
	mtime time.Time
}

func NewCredentialStore(base, account string) *CredentialStore {
	return &CredentialStore{base: base, account: account, ttl: tokenTTL}
}

func (s *CredentialStore) stamp(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Get 返回缓存凭据；TTL 内也查一次 mtime，桌面端换 token 后自动重解密。
func (s *CredentialStore) Get() (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred != nil && time.Since(s.at) < s.ttl {
		if s.mtime.IsZero() || s.stamp(s.cred.Path).Equal(s.mtime) {
			return s.cred, nil
		}
	}
	cred, err := DecryptCredential(s.base, s.account)
	if err != nil {
		return nil, err
	}
	s.cred = cred
	s.at = time.Now()
	s.mtime = s.stamp(cred.Path)
	return cred, nil
}

// Invalidate 丢弃缓存（上游 401 后重新解密——token 可能刚被桌面端轮换）。
func (s *CredentialStore) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = nil
	s.at = time.Time{}
}

// SetCached 把外部（refresh 续期）拿到的新凭据塞回缓存。否则 TTL 缓存里还是旧的，
// 下一个并发请求双检时又读到旧的 → 白白再换一次（实测 8 并发换了 2 次）。
// 借 store 自己的锁改，避免和它的热重载打架。
func (s *CredentialStore) SetCached(c *Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = c
	s.at = time.Now()
	s.mtime = s.stamp(c.Path)
}

// Cached 返回当前缓存快照（不触发解密），对应 Python 直接读 _store._cred 的用法。
func (s *CredentialStore) Cached() *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}
