package terminal

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"shellgo/internal/config"
)

// TestTermCwd 验证能读到 shell 的工作目录，并且 cd 之后跟着变。
// 实现依赖 /proc/<pid>/cwd，所以只在 Linux 上跑；其他平台只验证「明确报错」，
// 免得调用方拿到空字符串还以为是根目录。
func TestTermCwd(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("没有可用的 /bin/sh")
	}
	dir := t.TempDir()
	tc := &config.Terminal{Shell: "/bin/sh", Cwd: dir, Scrollback: 4096}
	term, err := newTerm(tc, config.Record{}, "tester", 80, 24, nil)
	if err != nil {
		t.Fatalf("创建终端失败：%v", err)
	}
	defer term.Close()

	if runtime.GOOS != "linux" {
		if p, err := term.Cwd(); err == nil {
			t.Fatalf("非 Linux 平台应返回错误，实际拿到 %q", p)
		}
		return
	}

	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	// shell 起来需要一点时间，轮询而不是盲等
	got := waitCwd(t, term, want)
	if got != want {
		t.Fatalf("初始 cwd = %q，期望 %q", got, want)
	}

	sub := filepath.Join(want, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := term.Write([]byte("cd " + sub + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := waitCwd(t, term, sub); got != sub {
		t.Fatalf("cd 之后 cwd = %q，期望 %q", got, sub)
	}
}

func waitCwd(t *testing.T, term *Term, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if p, err := term.Cwd(); err == nil {
			got = strings.TrimRight(p, "/")
			if got == "" {
				got = "/"
			}
			if got == want {
				return got
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return got
}

// TestTermCwdAfterClose 确保已关闭的终端不会返回上一个目录：
// 进程没了，/proc 里的链接也消失，此时必须报错而不是给出陈旧路径。
func TestTermCwdAfterClose(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("仅 Linux 有意义")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("没有可用的 /bin/sh")
	}
	tc := &config.Terminal{Shell: "/bin/sh", Scrollback: 1024}
	term, err := newTerm(tc, config.Record{}, "tester", 80, 24, nil)
	if err != nil {
		t.Fatalf("创建终端失败：%v", err)
	}
	term.Close()
	// Close 是先 SIGHUP、3 秒后才 SIGKILL，正常 shell 收到 SIGHUP 就退出，
	// 这里给足 5 秒，避免在慢机器上把「还没退出」误判成缺陷。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := term.Cwd(); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("终端关闭后 Cwd 应返回错误")
}
