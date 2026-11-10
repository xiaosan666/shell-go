package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlUnmarshal 让测试不必重复写一遍 Unmarshal 调用。
func yamlUnmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }

// TestLangPriority 固定 LANG 的三条优先级：
// 显式配置 > 继承进程环境 > 兜底 C.UTF-8。
// 写死 LANG 会把部署机上已有的中文 locale 覆盖掉，所以这里卡住行为。
func TestLangPriority(t *testing.T) {
	t.Run("继承环境", func(t *testing.T) {
		t.Setenv("LANG", "zh_CN.UTF-8")
		c := Default()
		c.normalize()
		if got := c.Terminal.Env["LANG"]; got != "zh_CN.UTF-8" {
			t.Fatalf("应继承环境 LANG，实际 %q", got)
		}
	})

	t.Run("环境为空时兜底", func(t *testing.T) {
		t.Setenv("LANG", "")
		c := Default()
		c.normalize()
		if got := c.Terminal.Env["LANG"]; got != "C.UTF-8" {
			t.Fatalf("环境无 LANG 时应兜底 C.UTF-8，实际 %q", got)
		}
	})

	t.Run("显式配置优先", func(t *testing.T) {
		t.Setenv("LANG", "en_US.UTF-8")
		c := Default()
		c.Terminal.Env["LANG"] = "zh_CN.UTF-8"
		c.normalize()
		if got := c.Terminal.Env["LANG"]; got != "zh_CN.UTF-8" {
			t.Fatalf("显式配置应优先，实际 %q", got)
		}
	})

	t.Run("总是注入 TERM", func(t *testing.T) {
		c := Default()
		c.normalize()
		if got := c.Terminal.Env["TERM"]; got != "xterm-256color" {
			t.Fatalf("TERM 应为 xterm-256color，实际 %q", got)
		}
	})
}

// TestCapsByRole 固定三种角色的默认权限。
// 这里的组合直接决定「能不能开终端、能不能删文件、能不能看别人的录制」，
// 改动会被 e2e 与单元测试同时卡住。
func TestCapsByRole(t *testing.T) {
	cases := []struct {
		role  string
		want  Caps
		title string
	}{
		{"", Caps{Role: RoleOperator, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: false, RecordsDelete: true}, "留空按 operator"},
		{"operator", Caps{Role: RoleOperator, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: false, RecordsDelete: true}, "operator"},
		{"admin", Caps{Role: RoleAdmin, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: true, RecordsDelete: true}, "admin"},
		{"viewer", Caps{Role: RoleViewer, Shell: false, FilesRead: true, FilesWrite: false,
			FilesDelete: false, RecordsAll: true, RecordsDelete: false}, "viewer"},
		{"  ADMIN  ", Caps{Role: RoleAdmin, Shell: true, FilesRead: true, FilesWrite: true,
			FilesDelete: true, RecordsAll: true, RecordsDelete: true}, "大小写与空格应被忽略"},
	}
	for _, c := range cases {
		got := User{Username: "u", Role: c.role}.CapsOf(8)
		c.want.MaxTerms = 8
		if got != c.want {
			t.Fatalf("%s：期望 %+v，实际 %+v", c.title, c.want, got)
		}
	}
}

// TestCapsPermOverride 检查 perms 能覆盖角色默认值，且未写的项仍沿用默认。
func TestCapsPermOverride(t *testing.T) {
	set := func(v bool) OptBool { return OptBool{Set: true, Val: v} }

	// 给运维账号开「看所有人的录制」，但不给他删别人录制的越权
	u := User{Username: "u", Role: RoleOperator, Perms: &Perms{RecordsAll: set(true)}}
	c := u.CapsOf(8)
	if !c.RecordsAll {
		t.Fatal("perms.records_all 应覆盖角色默认值")
	}
	if !c.Shell || !c.FilesWrite {
		t.Fatal("未覆盖的项应沿用角色默认值")
	}

	// 把管理员的终端权限显式关掉：这是「只读管理员」的用法
	u = User{Username: "u", Role: RoleAdmin, Perms: &Perms{Shell: set(false)}}
	c = u.CapsOf(8)
	if c.Shell {
		t.Fatal("perms.shell: false 应生效")
	}
	if !c.FilesDelete || !c.RecordsAll {
		t.Fatal("只关终端不应影响其他权限")
	}

	// 给 viewer 开终端：审计员临时排障
	u = User{Username: "u", Role: RoleViewer, Perms: &Perms{Shell: set(true), MaxTerms: 2}}
	c = u.CapsOf(8)
	if !c.Shell || c.MaxTerms != 2 {
		t.Fatalf("viewer 的覆盖不生效：%+v", c)
	}
	if c.FilesWrite {
		t.Fatal("viewer 默认不可写，未覆盖时不应变成可写")
	}
}

func TestCapsMaxTermsFallback(t *testing.T) {
	if got := (User{}).CapsOf(6).MaxTerms; got != 6 {
		t.Fatalf("未指定时应用全局上限，实际 %d", got)
	}
	if got := (User{Perms: &Perms{MaxTerms: 0}}).CapsOf(6).MaxTerms; got != 6 {
		t.Fatalf("max_terms 为 0 表示沿用全局，实际 %d", got)
	}
	if got := (User{Perms: &Perms{MaxTerms: 3}}).CapsOf(6).MaxTerms; got != 3 {
		t.Fatalf("指定 3 时应为 3，实际 %d", got)
	}
}

// TestOptBoolUnmarshal 保证「不写」与「写 false」被区分开——
// 这是权限覆盖能成立的前提，用普通 bool 做不到。
func TestOptBoolUnmarshal(t *testing.T) {
	var c Config
	yaml := `
users:
  - username: a
    password: x
    role: viewer
    perms:
      shell: true
      files_write: false
  - username: b
    password: y
`
	if err := yamlUnmarshal([]byte(yaml), &c); err != nil {
		t.Fatal(err)
	}
	a, b := c.Users[0], c.Users[1]
	if a.Perms == nil || !a.Perms.Shell.Set || !a.Perms.Shell.Val {
		t.Fatal("shell: true 应被解析为已设置且为 true")
	}
	if !a.Perms.FilesWrite.Set || a.Perms.FilesWrite.Val {
		t.Fatal("files_write: false 应被解析为已设置且为 false")
	}
	if a.Perms.FilesDelete.Set {
		t.Fatal("没写的项不应被标记为已设置，否则会误覆盖角色默认值")
	}
	if b.Perms != nil {
		t.Fatal("没有 perms 段时应为 nil")
	}
}

func TestValidateRole(t *testing.T) {
	base := func(role string) *Config {
		c := Default()
		c.Users = []User{{Username: "a", Password: "x", Role: role}}
		return c
	}
	if err := base("root").Validate(); err == nil {
		t.Fatal("未知角色应报错")
	}
	for _, r := range []string{"", "admin", "operator", "viewer", "Viewer"} {
		if err := base(r).Validate(); err != nil {
			t.Fatalf("角色 %q 应合法：%v", r, err)
		}
	}
	c := Default()
	c.Users = []User{{Username: "a", Password: "x", Perms: &Perms{MaxTerms: -1}}}
	if err := c.Validate(); err == nil {
		t.Fatal("max_terms 为负应报错")
	}
}

// TestExampleConfigLoads 保证仓库里的 config.example.yaml 始终可解析、可校验。
// 示例写坏了，照它部署的人会直接启动失败，而这类错误只有跑起来才会发现。
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("示例配置解析失败：%v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("示例配置校验失败：%v", err)
	}
	if len(c.Users) == 0 {
		t.Fatal("示例配置应至少含一个账号")
	}
	if got := c.Users[0].CapsOf(c.Security.MaxTerms); got.Role != RoleAdmin {
		t.Fatalf("示例里的引导账号应是 admin，实际 %s", got.Role)
	}
}
