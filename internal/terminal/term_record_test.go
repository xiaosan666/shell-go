package terminal

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shellgo/internal/config"
)

// TestTermWritesRecord 验证真实 PTY 的输出会落到录制文件里。
// 这是纯单元测试覆盖不到的部分：readLoop 是否把 chunk 交给 Recorder。
func TestTermWritesRecord(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("没有可用的 /bin/sh")
	}
	dir := t.TempDir()
	tc := &config.Terminal{Shell: "/bin/sh", Args: []string{"-i"}, Scrollback: 65536}
	rc := config.Record{Enabled: true, Dir: dir, MaxMB: 5}

	term, err := newTerm(tc, rc, "tester", 80, 24, nil)
	if err != nil {
		t.Fatalf("创建终端失败：%v", err)
	}
	term.Write([]byte("echo REC_INTEGRATION_OK\n"))
	// 等待 shell 输出 + 定时落盘（flushEvery=300ms）
	time.Sleep(800 * time.Millisecond)
	if term.Size() == 0 {
		t.Fatal("PTY 应有输出")
	}
	term.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("应有 1 个录制文件，实际 %d 个", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"end"`) {
		t.Error("关闭后应写入结束行")
	}
	meta, events, _, _, err := LoadRecord(dir, strings.TrimSuffix(filepath.Base(files[0]), ".jsonl"), 0, 100)
	if err != nil {
		t.Fatalf("读取录制失败：%v", err)
	}
	if len(events) == 0 {
		t.Fatalf("录制里应有事件，文件内容：%s", text)
	}
	if meta.Term != term.ID {
		t.Errorf("录制归属的终端不一致：%s vs %s", meta.Term, term.ID)
	}

	var all []byte
	for _, ev := range events {
		b, err := base64.StdEncoding.DecodeString(ev.D)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if !strings.Contains(string(all), "REC_INTEGRATION_OK") {
		t.Errorf("录制内容缺少命令回显，实际：%q", string(all))
	}
}
