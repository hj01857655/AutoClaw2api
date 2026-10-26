// 国内版手机验证码登录：端点 base 分流、路径形状、code 数字化、落盘激活。
// 全离线——httptest 假上游 + 内存假 DPAPI，绝不出网。
package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// xor5a 可逆假 DPAPI（与多账号测试同思路：只关心形状不关心强度）。
func xor5a(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0x5a
	}
	return out
}

func TestSmsLoginFlow(t *testing.T) {
	var gotPath, gotPhone, gotCode any
	var gotSrc string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPhone = body["phone"]
		gotCode = body["code"]
		gotSrc, _ = body["source_id"].(string)
		if body["device_id"] == "" {
			t.Error("device_id 缺失（上游 400001）")
		}
		writeJSON := func(token string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"access_token":  token,
				"refresh_token": "rt-" + token,
				"user_id":       "666001",
			}})
		}
		switch r.URL.Path {
		case "/userapi/v1/agent-send-code":
			writeJSON("stub")
		case "/userapi/v1/agent-login/":
			writeJSON("sms-tok")
		default:
			t.Errorf("未预期的路径: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	s := newMultiStore(t)
	s.Client.CNBase = up.URL // 测试直接指到假上游

	if err := s.SendSmsCode("13800138000"); err != nil {
		t.Fatalf("发码失败: %v", err)
	}
	if gotPath != "/userapi/v1/agent-send-code" || gotPhone != "13800138000" || gotSrc != "autoclaw" {
		t.Fatalf("发码请求形状不对: path=%v phone=%v src=%v", gotPath, gotPhone, gotSrc)
	}

	cred, err := s.LoginSms("13800138000", "123456")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if gotPath != "/userapi/v1/agent-login/" { // 尾部斜杠是端点形状的一部分
		t.Fatalf("登录路径应为 agent-login/（带尾部斜杠）: %v", gotPath)
	}
	// code 必须按数字发送——上游按类型校验，字符串回 400001
	if c, ok := gotCode.(float64); !ok || c != 123456 {
		t.Fatalf("code 应为数字 123456, got %#v", gotCode)
	}
	if cred.Account != "666001" || cred.Phone != "13800138000" {
		t.Fatalf("凭据字段不对: %+v", cred)
	}
	// 登录即落盘激活：新 Store 实例（模拟重启、装同一套假 DPAPI）按激活
	// 标记读到国内号的 token。
	s2 := NewStore(s.dir, "http://auth.invalid", up.URL, "2.1.0", "win",
		"100003", "testkey", nil, 8378, testLogger(t))
	s2.SetDPAPI(
		func(b []byte, _ string) ([]byte, error) { return xor5a(b), nil },
		func(b []byte) ([]byte, error) { return xor5a(b), nil },
	)
	tok, err := s2.Get()
	if err != nil || tok != "sms-tok" {
		t.Fatalf("重启后应按激活标记读到国内号, got %q err=%v", tok, err)
	}
}

func TestSmsCodeValueDigitized(t *testing.T) {
	if v := smsCodeValue("654321"); v != int64(654321) {
		t.Fatalf("全数字应转 int64, got %#v", v)
	}
	if v := smsCodeValue("12a456"); v != "12a456" {
		t.Fatalf("非数字应原样透传, got %#v", v)
	}
	if v := smsCodeValue(""); v != "" {
		t.Fatalf("空串应原样, got %#v", v)
	}
}
