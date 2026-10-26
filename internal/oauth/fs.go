// oauth 的本地小工具：UUID、原子写、URL 拆解。
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

const byteSep = filepath.Separator

// parsedURL 拆解后的 URL，字段语义对齐 Python urlparse 的返回值：
// Port 只在 URL 里**显式写了**端口时非空（Python 的 parsed.port 为 None）。
type parsedURL struct {
	Scheme string
	Host   string // 不含端口
	Port   string
	User   bool
}

func parseURL(raw string) (*parsedURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &parsedURL{
		Scheme: u.Scheme,
		Host:   u.Hostname(),
		Port:   u.Port(),
		User:   u.User != nil,
	}, nil
}

// newUUID4 标准带横线 UUID（服务端按 UUID 形状校验，别给裸 hex）。
func newUUID4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand 在 Windows 上是熵源充足的；真失败就退回 sha256(时间+路径)，
		// 至少保证形状合法且本次进程唯一
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", os.Getpid(), os.Getuid())))
		b = sum[:16]
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// writeFileAtomic 先写临时文件再 rename：Python 版同样的做法，避免半途被杀留下
// 截断文件（凭据文件截断 = 下次启动要重新授权）。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
