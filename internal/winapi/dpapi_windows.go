//go:build windows

// Package winapi 提供 Windows 平台原生能力的零依赖绑定。本文件对应 Python 版
// lanes/autoclaw.py 的 _dpapi_unprotect() 与 oauth.OAuthCredentialStore._protect()
// ——原先都走 pywin32 的 win32crypt，Go 版直接调 crypt32.dll，不引第三方库。
package winapi

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	modCrypt32  = syscall.NewLazyDLL("crypt32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCryptProtectData   = modCrypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = modCrypt32.NewProc("CryptUnprotectData")
	// LocalFree 在 kernel32（与 Python 版 ctypes 实测一致：crypt32 里没有它）
	procLocalFree = modKernel32.NewProc("LocalFree")
)

// cryptProtectUIForbidden = CRYPTPROTECT_UI_FORBIDDEN (0x1)：不弹任何 UI。
// Python 版两处调用都传了这个 flag；服务进程里弹授权框会直接挂死。
const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) dataBlob {
	if len(d) == 0 {
		// 空输入也要给非空指针，否则 CryptProtectData 当成非法参数
		return dataBlob{cbData: 0, pbData: new(byte)}
	}
	return dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

func (b dataBlob) copyOut() []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// CryptProtectData DPAPI 加密（密文绑定当前 Windows 用户与机器）。
//
// desc 传空即可：CRYPT_DATA_DESCRIPTOR.pszDataDesc 是 LPWSTR，而 pywin32 那边
// 传的是 str；这个字段只是人可读备注、不参与密钥派生，所以 Go 版一律传 NULL，
// 避免把 UTF-8 当 UTF-16 送进 API。
func CryptProtectData(data []byte, desc string) ([]byte, error) {
	_ = desc
	in := newBlob(data)
	var out dataBlob

	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0,
		0, 0, 0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, errors.New("CryptProtectData 失败: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.copyOut(), nil
}

// CryptUnprotectData DPAPI 解密。换用户/换机器会失败（这是特性，不是 bug）。
func CryptUnprotectData(data []byte) ([]byte, error) {
	in := newBlob(data)
	var out dataBlob

	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0,
		0, 0, 0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, errors.New("CryptUnprotectData 失败: " + err.Error())
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.copyOut(), nil
}
