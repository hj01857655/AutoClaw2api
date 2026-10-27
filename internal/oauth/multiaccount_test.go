// 多账号落盘行为：旧单文件迁移、异号并存、切换、删除。全离线——
// DPAPI 用内存假实现，刷新用假函数，绝不出网。
package oauth

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMultiStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s := NewStore(dir, "http://auth.invalid", "http://cn.auth.invalid", "2.1.0", "win",
		"100003", "testkey", nil, 8378, testLogger(t))
	// 假 DPAPI：xor 一字节，可逆即可（测试只关心形状不关心强度）
	s.SetDPAPI(
		func(b []byte, _ string) ([]byte, error) {
			out := make([]byte, len(b))
			for i, c := range b {
				out[i] = c ^ 0x5a
			}
			return out, nil
		},
		func(b []byte) ([]byte, error) {
			out := make([]byte, len(b))
			for i, c := range b {
				out[i] = c ^ 0x5a
			}
			return out, nil
		},
	)
	// 假刷新：到期也不出网
	s.SetRefresher(func(refreshToken, account string) (*Credential, error) {
		return &Credential{AccessToken: "fresh-" + refreshToken,
			RefreshToken: refreshToken, Account: account, ExpiresAt: ptrInt64(0)}, nil
	})
	return s
}

func ptrInt64(v int64) *int64 { return &v }

// 旧版单文件（无 active 标记、无分账号文件）必须还能读——升级网关不丢登录态。
func TestLegacySingleFileStillLoads(t *testing.T) {
	s := newMultiStore(t)
	legacy := &Credential{AccessToken: "tok-old", RefreshToken: "rt-old",
		Account: "legacyacct", ExpiresAt: ptrInt64(0)}
	s.SaveLogin(legacy)
	// 模拟旧版布局：凭据在 oauth-credential.bin，没有 active 标记
	if err := os.Rename(filepath.Join(s.dir, accountFile("legacyacct")), s.path); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(s.dir, ActiveFile))
	s.Invalidate()
	got, err := s.Get()
	if err != nil || got != "tok-old" {
		t.Fatalf("旧单文件应可读: got=%q err=%v", got, err)
	}
}

// 异号授权 → 两份凭据并存，且新号立即成为激活账号。
func TestTwoAccountsCoexist(t *testing.T) {
	s := newMultiStore(t)
	s.SaveLogin(&Credential{AccessToken: "tok-a", RefreshToken: "rt-a",
		Account: "111122223333", ExpiresAt: ptrInt64(0)})
	s.SaveLogin(&Credential{AccessToken: "tok-b", RefreshToken: "rt-b",
		Account: "444455556666", ExpiresAt: ptrInt64(0)})

	if got, _ := s.Get(); got != "tok-b" {
		t.Fatalf("授权后应立即用新号: got=%q", got)
	}
	list := s.ListAccounts()
	if len(list) != 2 {
		t.Fatalf("应有 2 个账号: %+v", list)
	}
	var activeAcct string
	n := 0
	for _, a := range list {
		if a.Active {
			n++
			activeAcct = a.Account
		}
	}
	if n != 1 || activeAcct != "444455556666" {
		t.Fatalf("应有且仅有最新号为激活: n=%d active=%q", n, activeAcct)
	}
}

// 切换：SelectAccount 后 Get 立刻返回旧号的 token；重启后仍记得选择。
func TestSelectAccountSwitches(t *testing.T) {
	s := newMultiStore(t)
	s.SaveLogin(&Credential{AccessToken: "tok-a", RefreshToken: "rt-a",
		Account: "111122223333", ExpiresAt: ptrInt64(0)})
	s.SaveLogin(&Credential{AccessToken: "tok-b", RefreshToken: "rt-b",
		Account: "444455556666", ExpiresAt: ptrInt64(0)})

	if err := s.SelectAccount("111122223333"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(); got != "tok-a" {
		t.Fatalf("切换后应用旧号 token: got=%q", got)
	}
	s.Invalidate() // 模拟重启：缓存清空，重新从盘上找
	if got, _ := s.Get(); got != "tok-a" {
		t.Fatalf("重启后应仍用选中的号: got=%q", got)
	}
}

// 同号重新授权 = 覆盖（仍是 2 个文件），token 换新。
func TestReauthSameAccountOverwrites(t *testing.T) {
	s := newMultiStore(t)
	s.SaveLogin(&Credential{AccessToken: "tok-a1", RefreshToken: "rt-a",
		Account: "111122223333", ExpiresAt: ptrInt64(0)})
	s.SaveLogin(&Credential{AccessToken: "tok-b", RefreshToken: "rt-b",
		Account: "444455556666", ExpiresAt: ptrInt64(0)})
	s.SaveLogin(&Credential{AccessToken: "tok-a2", RefreshToken: "rt-a2",
		Account: "111122223333", ExpiresAt: ptrInt64(0)})

	if got, _ := s.Get(); got != "tok-a2" {
		t.Fatalf("同号重授权应覆盖并切换: got=%q", got)
	}
	if len(s.ListAccounts()) != 2 {
		t.Fatalf("仍应只有 2 个账号: %+v", s.ListAccounts())
	}
}

// 删除激活账号 → 缓存清空、active 标记清除；另一个号还在，可再切换过去。
func TestDeleteAccount(t *testing.T) {
	s := newMultiStore(t)
	s.SaveLogin(&Credential{AccessToken: "tok-a", RefreshToken: "rt-a",
		Account: "111122223333", ExpiresAt: ptrInt64(0)})
	s.SaveLogin(&Credential{AccessToken: "tok-b", RefreshToken: "rt-b",
		Account: "444455556666", ExpiresAt: ptrInt64(0)})

	if err := s.DeleteAccount("444455556666"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(); err == nil {
		t.Fatal("删掉激活号后 Get 应报未登录")
	}
	if err := s.SelectAccount("111122223333"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(); got != "tok-a" {
		t.Fatalf("删号后应能切回剩余的号: got=%q", got)
	}
}

// 账号名带路径分隔符：文件名必须 sha256 化，不允许目录穿越。
func TestAccountFileNameNoTraversal(t *testing.T) {
	if got := accountFile("../../evil"); got == "" || filepath.Separator == '/' && got == "../../evil.bin" {
		t.Fatalf("文件名未消毒: %q", got)
	}
	if got := accountFile(""); got != "" {
		t.Fatalf("空账号应有空文件名: %q", got)
	}
	s := newMultiStore(t)
	s.SaveLogin(&Credential{AccessToken: "tok-x", Account: "..\\evil",
		ExpiresAt: ptrInt64(0)})
	if _, err := os.Stat(filepath.Join(s.dir, "..\\evil.bin")); err == nil {
		t.Fatal("目录穿越被写入")
	}
	if got, _ := s.Get(); got != "tok-x" {
		t.Fatalf("合法读取应不受影响: %q", got)
	}
}

// 保存时间戳：列表里能带出凭据新鲜度。
func TestSavedAtPopulated(t *testing.T) {
	s := newMultiStore(t)
	before := time.Now().Unix()
	s.SaveLogin(&Credential{AccessToken: "tok-a", Account: "111122223333",
		ExpiresAt: ptrInt64(0)})
	cred := s.loadFile(filepath.Join(s.dir, accountFile("111122223333")))
	if cred == nil || int64(cred.SavedAt) < before {
		t.Fatalf("savedAt 应落盘: %+v", cred)
	}
}

// SaveLogin 必须同时更新激活标记：否则重启后 loadCurrent 按旧标记读回旧账号，
// 实测症状是新登录 token 打 chat 401，手动 select 后才恢复。
func TestSaveLoginActivates(t *testing.T) {
	// 同一目录两个 Store 实例：第二个模拟重启后的进程
	dir := t.TempDir()
	mk := func() *Store {
		s := NewStore(dir, "http://auth.invalid", "http://cn.auth.invalid", "2.1.0", "win",
			"100003", "testkey", nil, 8378, testLogger(t))
		s.SetDPAPI(
			func(b []byte, _ string) ([]byte, error) {
				out := make([]byte, len(b))
				for i, c := range b {
					out[i] = c ^ 0x5a
				}
				return out, nil
			},
			func(b []byte) ([]byte, error) {
				out := make([]byte, len(b))
				for i, c := range b {
					out[i] = c ^ 0x5a
				}
				return out, nil
			},
		)
		return s
	}

	s := mk()
	s.SaveLogin(&Credential{AccessToken: "tok-new", RefreshToken: "rt-new", Account: "b280d91248f0", ExpiresAt: ptrInt64(0)})

	s2 := mk()
	tok, err := s2.Get()
	if err != nil || tok != "tok-new" {
		t.Fatalf("重启后应读到新登录的账号, got %q err=%v", tok, err)
	}
	if got := s2.Cached(); got == nil || got.Account != "b280d91248f0" {
		t.Fatalf("激活账号应为 b280d91248f0, got %+v", got)
	}
}

// 批量刷新：只续临期账号、不改激活标记、激活账号同步内存缓存。
func TestRefreshAllExpiringKeepsActive(t *testing.T) {
	s := newMultiStore(t)
	soon := time.Now().Add(30 * time.Minute).Unix() // 30 分钟后到期 → 临期
	far := time.Now().Add(48 * time.Hour).Unix()
	a := &Credential{AccessToken: "tok-a", RefreshToken: "rt-a", Account: "aaa", ExpiresAt: ptrInt64(soon)}
	b := &Credential{AccessToken: "tok-b", RefreshToken: "rt-b", Account: "bbb", ExpiresAt: ptrInt64(far)}
	s.SaveLogin(a)
	// SaveLogin(b) 会把激活切到 b；手工切回 a 模拟 a 是激活账号
	s.SaveLogin(b)
	if err := s.SelectAccount("aaa"); err != nil {
		t.Fatal(err)
	}

	n := s.RefreshAllExpiring(time.Hour)
	if n != 1 {
		t.Fatalf("应只刷新临期的 1 个账号，实际 %d", n)
	}
	// 激活标记没被批量刷新带走
	active := readTrim(filepath.Join(s.dir, ActiveFile))
	if active != "aaa" {
		t.Fatalf("批量刷新不应改激活标记: active=%q", active)
	}
	// 临期账号（aaa，30 分钟后到期）已换成新 token
	cred := s.loadFile(filepath.Join(s.dir, accountFile("aaa")))
	if cred == nil || cred.AccessToken != "fresh-rt-a" {
		t.Fatalf("aaa 应已刷新: %+v", cred)
	}
	// 未临期（bbb，48 小时后到期）的原样
	cred = s.loadFile(filepath.Join(s.dir, accountFile("bbb")))
	if cred == nil || cred.AccessToken != "tok-b" {
		t.Fatalf("bbb 未临期不应动: %+v", cred)
	}
}

// 代理表：全名与 12 位前缀都能命中，未配置直连。
func TestProxyForMatchesPrefix(t *testing.T) {
	s := newMultiStore(t)
	s.SetProxies(map[string]string{
		"569867b9a86e":     "http://127.0.0.1:7890", // 12 位前缀
		"full-account-xyz": "socks5://p:1080",       // 全名
	})
	if got := s.ProxyFor("569867b9a86eabcdef99"); got != "http://127.0.0.1:7890" {
		t.Fatalf("前缀未命中: %q", got)
	}
	if got := s.ProxyFor("full-account-xyz"); got != "socks5://p:1080" {
		t.Fatalf("全名未命中: %q", got)
	}
	if got := s.ProxyFor("unknown"); got != "" {
		t.Fatalf("未配置应直连: %q", got)
	}
	if got := s.ProxyFor(""); got != "" {
		t.Fatalf("空账号应直连: %q", got)
	}
}
