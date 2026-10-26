// Package config 装配 AutoClaw2api 的运行配置：.env 加载 + 上游常量 + 状态目录。
//
// 状态目录规则与 Python 版对齐（Python: dirname(abspath(server.py))）：
// panel.html、request.log、dumps/、.cred/ 全部落在状态目录。
// 部署时把 exe 放进原 AutoClaw2api 目录（或设 AUTOCLAW2API_STATE_DIR 指向它），
// 即可与 Python 版交替读写同一套磁盘文件。
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// 上游常量（对应 lanes/autoclaw.py 顶部，全部来自 main.cjs 实证，非猜测）。
const (
	AppID             = "100003"
	DefaultAppKey     = "38d2391985e2369a5fb8227d8e6cd5e5" // 环境变量可覆盖，留默认值兜底
	DefaultUpstream   = "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw"
	UpstreamPath      = "/chat/completions"
	DefaultAccount    = "https://autoglm-acceleration-api.zhipuai.cn" // 账号面裸域名（与模型网关不同 base）
	DefaultAuthBase   = "https://autoglm-acceleration-api.zhipuai.cn"
	DefaultCNAuthBase = "https://autoglm-api.zhipuai.cn"                                            // 国内版账号面（手机验证码登录端点只挂国内域）
	DefaultCNUpstream = "https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw" // 国内版 chat（managed gateway，路径无 /v1）
	CNUpstreamPath    = "/chat/completions"
	DefaultModel      = "zai_auto"
	DefaultAppVersion = "2.1.0"
	ListenHost        = "127.0.0.1" // 凭据绑死本机 DPAPI，默认仅回环
	DefaultPort       = 8378
)

type Config struct {
	Host             string
	APIKey           string
	Port             int
	StateDir         string // panel.html / request.log / dumps/ / .cred 所在
	DumpOn           bool
	DumpDir          string
	DumpMaxFiles     int
	AppVersion       string
	Platform         string // managedPlatform()：win32->win, darwin->mac
	AppKey           string
	UpstreamBase     string
	CNUpstreamBase   string
	AccountBase      string // 账号面 base（签到/余额，裸域名）
	DefaultModelName string
	OAuthDir         string // oauth 凭据目录（默认 <状态目录>/.cred）
	AuthBase         string // oauth 端点 base
	CNAuthBase       string // 国内版账号面 base（验证码登录）
	OAuthExtraHosts  []string
	AutoClaim        bool // 每日自动签到（daily_signin，默认开）
}

// StateFilePath 返回状态目录下的文件路径。
func (c *Config) StateFilePath(name string) string { return filepath.Join(c.StateDir, name) }

// Load 读取配置。优先级：环境变量 > .env > 默认值。
// .env 从状态目录和工作目录两处尝试（与 python-dotenv 的 load_dotenv 语义近似；
// Python 版里 load_dotenv() 必须先于 lanes 导入，否则 AUTOCLAW_APP_KEY 的
// .env 覆盖值无效——Go 版所有配置集中在 Load() 里读，天然没有这个顺序问题）。
func Load() *Config {
	c := &Config{Port: DefaultPort}
	c.StateDir = os.Getenv("AUTOCLAW2API_STATE_DIR")
	if c.StateDir == "" {
		if exe, err := os.Executable(); err == nil {
			c.StateDir = filepath.Dir(exe)
		} else {
			wd, _ := os.Getwd()
			c.StateDir = wd
		}
	}
	for _, dir := range []string{c.StateDir, mustWd()} {
		loadDotEnv(filepath.Join(dir, ".env"))
	}

	c.Host = orDefault(os.Getenv("HOST"), ListenHost)
	c.APIKey = strings.TrimSpace(os.Getenv("API_KEY"))
	if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PORT"))); err == nil && p > 0 {
		c.Port = p
	}
	c.DumpOn = strings.TrimSpace(os.Getenv("DUMP")) != "0"
	if v := strings.TrimSpace(os.Getenv("DUMP_MAX_FILES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DumpMaxFiles = n
		}
	} else {
		c.DumpMaxFiles = 200
	}
	c.DumpDir = strings.TrimSpace(os.Getenv("DUMP_DIR"))
	if c.DumpDir == "" {
		c.DumpDir = filepath.Join(c.StateDir, "dumps")
	}

	c.AppVersion = os.Getenv("AUTOCLAW_APP_VERSION")
	if c.AppVersion == "" {
		c.AppVersion = DefaultAppVersion
	}
	c.Platform = os.Getenv("AUTOCLAW_PLATFORM")
	if c.Platform == "" {
		if runtime.GOOS == "windows" {
			c.Platform = "win"
		} else {
			c.Platform = "mac"
		}
	}
	c.AppKey = strings.TrimSpace(os.Getenv("AUTOCLAW_APP_KEY"))
	if c.AppKey == "" {
		c.AppKey = DefaultAppKey
	}
	c.UpstreamBase = strings.TrimRight(
		orDefault(os.Getenv("AUTOCLAW_UPSTREAM_BASE"), DefaultUpstream), "/")
	c.CNUpstreamBase = strings.TrimRight(
		orDefault(os.Getenv("AUTOCLAW_CN_UPSTREAM_BASE"), DefaultCNUpstream), "/")
	c.AccountBase = strings.TrimRight(orDefault(os.Getenv("AUTOCLAW_ACCOUNT_BASE"), DefaultAccount), "/")
	c.DefaultModelName = orDefault(os.Getenv("AUTOCLAW_DEFAULT_MODEL"), DefaultModel)
	c.OAuthDir = os.Getenv("AUTOCLAW_OAUTH_DIR")
	if c.OAuthDir == "" {
		c.OAuthDir = filepath.Join(c.StateDir, ".cred")
	}
	c.AuthBase = strings.TrimRight(orDefault(os.Getenv("AUTOCLAW_AUTH_BASE"), DefaultAuthBase), "/")
	c.CNAuthBase = strings.TrimRight(orDefault(os.Getenv("AUTOCLAW_CN_AUTH_BASE"), DefaultCNAuthBase), "/")
	// 默认开；设 0/off/false 关（签到幂等，重复调只是 Already）
	c.AutoClaim = true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AUTOCLAW_AUTO_CLAIM"))) {
	case "0", "off", "false":
		c.AutoClaim = false
	}
	for _, h := range strings.Split(os.Getenv("AUTOCLAW_OAUTH_EXTRA_HOSTS"), ",") {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			c.OAuthExtraHosts = append(c.OAuthExtraHosts, h)
		}
	}
	return c
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func mustWd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// loadDotEnv 极简 .env 解析：KEY=VALUE 行，# 注释，不展开变量，不覆盖已有 env。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
