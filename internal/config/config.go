package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持 "30m" / "12h" / 300 这样的 YAML 写法。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			*d = 0
			return nil
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("无效的时长 %q，示例：30m、12h、90s", s)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := value.Decode(&n); err != nil {
		return fmt.Errorf("无效的时长：%s", value.Value)
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type TLS struct {
	Enabled bool   `yaml:"enabled"`
	Cert    string `yaml:"cert"`
	Key     string `yaml:"key"`
	// Auto 为 true 且 cert/key 为空时，启动时自动生成自签证书（仅内存，不落盘）。
	Auto bool `yaml:"auto"`
}

// OptBool 是三态布尔：区分「没写」和「写了 false」。
// 权限覆盖需要这个区分——不写表示沿用角色默认值，写了 false 才是显式关闭。
type OptBool struct {
	Set bool
	Val bool
}

func (o *OptBool) UnmarshalYAML(v *yaml.Node) error {
	var b bool
	if err := v.Decode(&b); err != nil {
		return fmt.Errorf("需要 true 或 false，实际是 %q", v.Value)
	}
	o.Set, o.Val = true, b
	return nil
}

// Or 未设置时返回 def。
func (o OptBool) Or(def bool) bool {
	if o.Set {
		return o.Val
	}
	return def
}

// 角色预设。逐项权限可在 perms 里单独覆盖。
const (
	RoleAdmin    = "admin"    // 全权：终端 + 文件增删改 + 查看/删除所有人的录制
	RoleOperator = "operator" // 日常运维：终端 + 文件增删改 + 只能看自己的录制
	RoleViewer   = "viewer"   // 审计只读：不能开终端，只能读文件、看所有人的录制
)

// Caps 是一个账号最终生效的能力集合。
type Caps struct {
	Role          string `json:"role"`
	Shell         bool   `json:"shell"`         // 能否新建终端
	FilesRead     bool   `json:"filesRead"`     // 浏览/下载
	FilesWrite    bool   `json:"filesWrite"`    // 上传/新建/重命名/在线编辑
	FilesDelete   bool   `json:"filesDelete"`   // 删除
	RecordsAll    bool   `json:"recordsAll"`    // 能否查看所有人的会话录制
	RecordsDelete bool   `json:"recordsDelete"` // 能否删除录制
	MaxTerms      int    `json:"maxTerms"`      // 该账号可同时打开的终端数
}

// Perms 逐项覆盖角色默认值。字段留空即沿用角色默认。
type Perms struct {
	Shell         OptBool `yaml:"shell"`
	FilesRead     OptBool `yaml:"files_read"`
	FilesWrite    OptBool `yaml:"files_write"`
	FilesDelete   OptBool `yaml:"files_delete"`
	RecordsAll    OptBool `yaml:"records_all"`
	RecordsDelete OptBool `yaml:"records_delete"`
	MaxTerms      int     `yaml:"max_terms"`
}

type User struct {
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordHash string `yaml:"password_hash"`
	Role         string `yaml:"role"`  // admin | operator | viewer，留空按 operator
	Perms        *Perms `yaml:"perms"` // 可选，逐项微调
}

// CapsOf 计算账号的最终能力：角色给默认值 → perms 覆盖 → 补上全局终端上限。
// 它不处理 files.allow_write 这类全局开关，那是运行期与配置的交集，由调用方判断。
func (u User) CapsOf(globalMaxTerms int) Caps {
	var c Caps
	switch strings.ToLower(strings.TrimSpace(u.Role)) {
	case RoleAdmin:
		c = Caps{Role: RoleAdmin, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: true, RecordsDelete: true}
	case RoleViewer:
		// 审计角色：看得见所有录制和文件，但不能动手。
		c = Caps{Role: RoleViewer, Shell: false, FilesRead: true, FilesWrite: false,
			FilesDelete: false, RecordsAll: true, RecordsDelete: false}
	default:
		c = Caps{Role: RoleOperator, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: false, RecordsDelete: true}
	}
	c.MaxTerms = globalMaxTerms
	if p := u.Perms; p != nil {
		c.Shell = p.Shell.Or(c.Shell)
		c.FilesRead = p.FilesRead.Or(c.FilesRead)
		c.FilesWrite = p.FilesWrite.Or(c.FilesWrite)
		c.FilesDelete = p.FilesDelete.Or(c.FilesDelete)
		c.RecordsAll = p.RecordsAll.Or(c.RecordsAll)
		c.RecordsDelete = p.RecordsDelete.Or(c.RecordsDelete)
		if p.MaxTerms > 0 {
			c.MaxTerms = p.MaxTerms
		}
	}
	return c
}

type Security struct {
	IPWhitelist  []string `yaml:"ip_whitelist"`
	IdleTimeout  Duration `yaml:"idle_timeout"`
	SessionTTL   Duration `yaml:"session_ttl"`
	MaxSessions  int      `yaml:"max_sessions"`
	MaxTerms     int      `yaml:"max_terms"`
	LoginMaxFail int      `yaml:"login_max_fail"`
	LoginLock    Duration `yaml:"login_lock"`
	AllowRoot    bool     `yaml:"allow_root"`
	// 允许跨源请求（默认关闭）
	AllowCORS bool `yaml:"allow_cors"`
}

type Terminal struct {
	Shell      string            `yaml:"shell"`
	Args       []string          `yaml:"args"`
	Cwd        string            `yaml:"cwd"`
	Env        map[string]string `yaml:"env"`
	Scrollback int               `yaml:"scrollback"`
	// ws：WebSocket 实时通道；poll：纯 POST 轮询
	Transport   string   `yaml:"transport"`
	Transport2  string   `yaml:"-"`
	PollMs      int      `yaml:"poll_interval_ms"`
	IdleClose   Duration `yaml:"idle_close"`
	SnapshotMax int      `yaml:"snapshot_max"`
	Title       string   `yaml:"title"`
}

// Files 控制网页端文件管理的可见范围与权限。
// 所有路径都被限制在 root 之下，符号链接逃逸会被拒绝。
type Files struct {
	Enabled     bool   `yaml:"enabled"`
	Root        string `yaml:"root"`          // 允许访问的根目录，支持 ~ 与相对路径
	MaxUploadMB int    `yaml:"max_upload_mb"` // 单文件上传上限
	MaxEditKB   int    `yaml:"max_edit_kb"`   // 网页内在线编辑的文本大小上限
	AllowWrite  bool   `yaml:"allow_write"`   // 允许上传/新建/重命名
	AllowDelete bool   `yaml:"allow_delete"`  // 允许删除
}

// Record 是终端会话录制（回放）配置。
type Record struct {
	Enabled  bool   `yaml:"enabled"`
	Dir      string `yaml:"dir"`
	MaxMB    int    `yaml:"max_mb"`    // 单个会话录制的体积上限
	KeepDays int    `yaml:"keep_days"` // 保留天数，0 表示不按时间清理
	MaxFiles int    `yaml:"max_files"` // 最多保留文件数，0 表示不限制
}

type Audit struct {
	Enabled bool   `yaml:"enabled"`
	LogFile string `yaml:"log_file"`
}

type Log struct {
	File  string `yaml:"file"`
	Level string `yaml:"level"`
}

type Config struct {
	Listen   string   `yaml:"listen"`
	TLS      TLS      `yaml:"tls"`
	Users    []User   `yaml:"users"`
	Security Security `yaml:"security"`
	Terminal Terminal `yaml:"terminal"`
	Files    Files    `yaml:"files"`
	Record   Record   `yaml:"record"`
	Audit    Audit    `yaml:"audit"`
	Log      Log      `yaml:"log"`
}

func Default() *Config {
	return &Config{
		Listen: "0.0.0.0:8443",
		TLS:    TLS{Enabled: false, Auto: true},
		Security: Security{
			IdleTimeout:  Duration(30 * time.Minute),
			SessionTTL:   Duration(12 * time.Hour),
			MaxSessions:  5,
			MaxTerms:     8,
			LoginMaxFail: 5,
			LoginLock:    Duration(10 * time.Minute),
		},
		Terminal: Terminal{
			Shell:       "/bin/bash",
			Args:        []string{"-l"},
			Cwd:         "",
			Env:         map[string]string{"TERM": "xterm-256color"},
			Scrollback:  512 * 1024,
			Transport:   "ws",
			PollMs:      120,
			IdleClose:   Duration(30 * time.Minute),
			SnapshotMax: 128 * 1024,
			Title:       "Shell Go",
		},
		Files: Files{
			Enabled:     true,
			Root:        "~",
			MaxUploadMB: 200,
			MaxEditKB:   1024,
			AllowWrite:  true,
			AllowDelete: true,
		},
		Record: Record{Enabled: true, Dir: "data/records", MaxMB: 50, KeepDays: 7, MaxFiles: 200},
		Audit:  Audit{Enabled: true, LogFile: "data/audit.log"},
		Log:    Log{File: "data/server.log", Level: "info"},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败：%w", err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败：%w", err)
		}
	}
	cfg.normalize()
	return cfg, nil
}

func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8443"
	}
	t := &c.Terminal
	if t.Shell == "" {
		t.Shell = "/bin/bash"
	}
	if t.Scrollback <= 0 {
		t.Scrollback = 512 * 1024
	}
	if t.Transport == "" {
		t.Transport = "ws"
	}
	t.Transport = strings.ToLower(t.Transport)
	if t.Transport != "poll" {
		t.Transport = "ws"
	}
	if t.PollMs < 40 {
		t.PollMs = 120
	}
	if t.SnapshotMax <= 0 {
		t.SnapshotMax = 128 * 1024
	}
	if t.Title == "" {
		t.Title = "Shell Go"
	}
	if t.Env == nil {
		t.Env = map[string]string{}
	}
	if _, ok := t.Env["TERM"]; !ok {
		t.Env["TERM"] = "xterm-256color"
	}
	// LANG 不写死：进程环境里有就继承（保留部署机的中文 locale），
	// 只在完全没有时补一个 UTF-8，避免 POSIX locale 下中文显示成问号。
	if _, ok := t.Env["LANG"]; !ok {
		if v := os.Getenv("LANG"); v != "" {
			t.Env["LANG"] = v
		} else {
			t.Env["LANG"] = "C.UTF-8"
		}
	}
	s := &c.Security
	if s.IdleTimeout <= 0 {
		s.IdleTimeout = Duration(30 * time.Minute)
	}
	if s.SessionTTL <= 0 {
		s.SessionTTL = Duration(12 * time.Hour)
	}
	if s.MaxSessions <= 0 {
		s.MaxSessions = 5
	}
	if s.MaxTerms <= 0 {
		s.MaxTerms = 8
	}
	if s.LoginMaxFail <= 0 {
		s.LoginMaxFail = 5
	}
	if s.LoginLock <= 0 {
		s.LoginLock = Duration(10 * time.Minute)
	}
	f := &c.Files
	if f.Root == "" {
		f.Root = "~"
	}
	if f.MaxUploadMB <= 0 {
		f.MaxUploadMB = 200
	}
	if f.MaxUploadMB > 4096 {
		f.MaxUploadMB = 4096
	}
	r := &c.Record
	if r.Dir == "" {
		r.Dir = "data/records"
	}
	if r.MaxMB <= 0 {
		r.MaxMB = 50
	}
	if r.KeepDays < 0 {
		r.KeepDays = 0
	}
	if r.MaxFiles < 0 {
		r.MaxFiles = 0
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}

// Validate 做启动前校验，返回 error 时应当直接退出。
func (c *Config) Validate() error {
	if len(c.Users) == 0 {
		return fmt.Errorf("未配置任何用户：请在 config.yaml 的 users 中配置，或使用 -user/-pass 启动")
	}
	seen := map[string]bool{}
	for i, u := range c.Users {
		if strings.TrimSpace(u.Username) == "" {
			return fmt.Errorf("users[%d] 用户名为空", i)
		}
		if seen[u.Username] {
			return fmt.Errorf("用户名重复：%s", u.Username)
		}
		seen[u.Username] = true
		if u.Password == "" && u.PasswordHash == "" {
			return fmt.Errorf("用户 %s 缺少 password 或 password_hash", u.Username)
		}
		switch strings.ToLower(strings.TrimSpace(u.Role)) {
		case "", RoleAdmin, RoleOperator, RoleViewer:
		default:
			return fmt.Errorf("用户 %s 的 role 只能是 admin / operator / viewer，实际是 %q", u.Username, u.Role)
		}
		if u.Perms != nil && u.Perms.MaxTerms < 0 {
			return fmt.Errorf("用户 %s 的 perms.max_terms 不能为负数", u.Username)
		}
	}
	for _, ip := range c.Security.IPWhitelist {
		if _, _, err := net.ParseCIDR(ip); err != nil {
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("ip_whitelist 中的 %q 既不是合法 IP 也不是 CIDR", ip)
			}
		}
	}
	if c.TLS.Enabled && !c.TLS.Auto {
		if c.TLS.Cert == "" || c.TLS.Key == "" {
			return fmt.Errorf("tls.enabled 为 true 时必须提供 cert 与 key，或设置 tls.auto: true")
		}
	}
	return nil
}

// AddUserFromFlags 用于 -user/-pass 覆盖式添加账号。
// 命令行给的是引导账号，按 admin 处理——否则用参数起的服务会连终端都开不了。
func (c *Config) AddUserFromFlags(user, pass string) {
	if user == "" {
		return
	}
	c.Users = []User{{Username: user, Password: pass, Role: RoleAdmin}}
}
