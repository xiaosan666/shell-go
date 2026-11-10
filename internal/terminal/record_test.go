package terminal

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shellgo/internal/config"
)

func TestRecordAndReplay(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Record{Enabled: true, Dir: dir, MaxMB: 1}

	r, err := newRecorder(cfg, "0123456789abcdef", "admin", 120, 32)
	if err != nil {
		t.Fatal(err)
	}
	r.WriteChunk([]byte("hello "))
	time.Sleep(10 * time.Millisecond)
	r.WriteChunk([]byte("世界\r\n"))
	r.Close()

	list, err := ListRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("期望 1 条录制，实际 %d", len(list))
	}
	m := list[0]
	if m.User != "admin" || m.Cols != 120 || m.Rows != 32 {
		t.Fatalf("元信息异常：%+v", m)
	}
	if m.End <= 0 {
		t.Fatalf("关闭后应有时长：%+v", m)
	}

	meta, events, next, eof, err := LoadRecord(dir, m.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != m.ID {
		t.Fatalf("ID 不一致：%s != %s", meta.ID, m.ID)
	}
	if len(events) != 2 || !eof || next != 2 {
		t.Fatalf("事件读取异常：len=%d eof=%v next=%d", len(events), eof, next)
	}
	var got string
	for _, ev := range events {
		b, err := base64.StdEncoding.DecodeString(ev.D)
		if err != nil {
			t.Fatal(err)
		}
		got += string(b)
	}
	if got != "hello 世界\r\n" {
		t.Fatalf("回放内容不一致：%q", got)
	}

	// 分页：第一页只取 1 条时不应误判为结束。
	_, page, next2, eof2, err := LoadRecord(dir, m.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || eof2 || next2 != 1 {
		t.Fatalf("分页异常：len=%d eof=%v next=%d", len(page), eof2, next2)
	}
}

func TestRecordIDValidation(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../etc/passwd", "abc", "20260101-000000_zzzz"} {
		if _, err := recordPath(dir, id); err == nil {
			t.Fatalf("非法 ID %q 应被拒绝", id)
		}
	}
}

func TestRecordSizeLimit(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Record{Enabled: true, Dir: dir, MaxMB: 1}
	r, err := newRecorder(cfg, "0123456789abcdef", "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 400*1024)
	for i := 0; i < 4; i++ {
		r.WriteChunk(chunk)
	}
	r.Close()

	fi, err := os.Stat(filepath.Join(dir, r.meta.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// 上限 1MB，写入 1.6MB 后应停止增长（允许一行元数据与结束行的余量）。
	if fi.Size() > 1100*1024 {
		t.Fatalf("录制超过上限仍在增长：%d", fi.Size())
	}
	if _, err := os.Stat(filepath.Join(dir, r.meta.ID+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanRecordsByCount(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Record{Enabled: true, Dir: dir, MaxMB: 1}
	for i := 0; i < 5; i++ {
		r, err := newRecorder(cfg, "0123456789abcde"+string(rune('0'+i)), "admin", 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteChunk([]byte("x"))
		r.Close()
	}
	removed := CleanRecords(dir, 0, 2)
	if removed != 3 {
		t.Fatalf("期望删除 3 个，实际 %d", removed)
	}
	list, _ := ListRecords(dir)
	if len(list) != 2 {
		t.Fatalf("期望保留 2 个，实际 %d", len(list))
	}
}
