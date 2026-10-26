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
	s := NewStore(dir, "http://auth.invalid", "2.1.0", "win",
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
