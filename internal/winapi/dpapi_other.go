//go:build !windows

// 非 Windows 平台：DPAPI 不存在。Python 版在这里抛 RuntimeError 让调用方给出
// 可操作的报错（"该机制仅适用于 Windows 同用户身份运行"），Go 版同样返回错误，
// 不静默降级成明文存储。
package winapi

import "errors"

var errUnsupported = errors.New("DPAPI 仅适用于 Windows（同用户身份运行）；当前平台无法解密 AutoClaw 凭据")

func CryptProtectData(data []byte, desc string) ([]byte, error) { return nil, errUnsupported }

func CryptUnprotectData(data []byte) ([]byte, error) { return nil, errUnsupported }
