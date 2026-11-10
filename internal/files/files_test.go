package files

import (
	"os"
	"path/filepath"
	"testing"

	"shellgo/internal/config"
)

func newTestManager(t *testing.T, mutate func(*config.Files)) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Files{Enabled: true, Root: root, MaxUploadMB: 1, AllowWrite: true, AllowDelete: true}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

func TestPathEscapeIsRejected(t *testing.T) {
	m, root := newTestManager(t, nil)
	outside := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	cases := []string{"../secret.txt", "../../etc/passwd", "/etc/passwd", "a/../../secret.txt"}
	for _, c := range cases {
		if _, _, err := m.resolve(c, true); err == nil {
			t.Fatalf("路径 %q 应当被拒绝", c)
		}
	}
}

func TestSymlinkEscapeIsRejected(t *testing.T) {
	m, root := newTestManager(t, nil)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skip("当前系统不允许创建符号链接")
	}
	if _, _, err := m.resolve("escape/x", true); err == nil {
		t.Fatal("符号链接逃逸应当被拒绝")
	}
}

func TestListAndChunkedWrite(t *testing.T) {
	m, root := newTestManager(t, nil)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, entries, err := m.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Size != 5 {
		t.Fatalf("列表结果异常：%+v", entries)
	}

	// 分两块写入，模拟前端上传。
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteChunk("sub/new.bin", 0, 9, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	next, err := m.WriteChunk("sub/new.bin", 4, 9, []byte("efghi"))
	if err != nil {
		t.Fatal(err)
	}
	if next != 9 {
		t.Fatalf("期望写入 9 字节，实际 %d", next)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub/new.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdefghi" {
		t.Fatalf("内容不一致：%q", got)
	}

	// 偏移不连续必须报错，避免拼接错位。
	if _, err := m.WriteChunk("sub/new.bin", 2, 9, []byte("zz")); err == nil {
		t.Fatal("偏移不连续应当报错")
	}
}

func TestUploadLimit(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if _, err := m.WriteChunk("big.bin", 0, 2<<20, []byte("x")); err == nil {
		t.Fatal("超过上限应当报错")
	}
}

func TestRemoveAndReadOnly(t *testing.T) {
	m, root := newTestManager(t, func(c *config.Files) { c.AllowDelete = false; c.AllowWrite = false })
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	if err := m.Remove("a.txt", false); err == nil {
		t.Fatal("禁止删除时不应删除成功")
	}
	if _, err := m.WriteChunk("b.txt", 0, 1, []byte("x")); err == nil {
		t.Fatal("只读时不应写入成功")
	}
}

func TestRenameRejectsSeparators(t *testing.T) {
	m, root := newTestManager(t, nil)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	for _, name := range []string{"../b", "x/y", "..", ""} {
		if _, err := m.Rename("a.txt", name); err == nil {
			t.Fatalf("名称 %q 应当被拒绝", name)
		}
	}
	next, err := m.Rename("a.txt", "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if next != "b.txt" {
		t.Fatalf("重命名路径异常：%s", next)
	}
}

func TestSaveTextRejectsStaleMtime(t *testing.T) {
	m, root := newTestManager(t, nil)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1"), 0o644)

	_, mt, err := m.StatDetail("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// 有人在我们读取之后改过文件
	if _, _, err := m.SaveText("a.txt", []byte("v2"), mt-1); err != ErrConflict {
		t.Fatalf("修改时间不匹配时应返回冲突，实际：%v", err)
	}
	if _, _, err := m.SaveText("a.txt", []byte("v2"), 0); err != ErrConflict {
		t.Fatalf("目标已存在但客户端认为是新建，应返回冲突，实际：%v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "v1" {
		t.Fatalf("冲突时不应写入，实际内容：%s", data)
	}

	size, next, err := m.SaveText("a.txt", []byte("v2 中文"), mt)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len("v2 中文")) || next == 0 {
		t.Fatalf("返回值异常：size=%d mtime=%d", size, next)
	}
	data, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "v2 中文" {
		t.Fatalf("保存内容不正确：%s", data)
	}
}

func TestSaveTextKeepsPermissionsAndLeavesNoTemp(t *testing.T) {
	m, root := newTestManager(t, nil)
	path := filepath.Join(root, "conf.conf")
	os.WriteFile(path, []byte("a=1"), 0o600)
	_, mt, _ := m.StatDetail("conf.conf")
	if _, _, err := m.SaveText("conf.conf", []byte("a=2"), mt); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("保存后权限被改成 %v", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 {
		t.Fatalf("残留了临时文件：%v", ents)
	}
}

func TestSaveTextRejectsBinaryAndOversize(t *testing.T) {
	m, root := newTestManager(t, func(c *config.Files) { c.MaxEditKB = 1 })
	if _, _, err := m.SaveText("bin.dat", []byte{'a', 0, 'b'}, 0); err != ErrBinary {
		t.Fatalf("含 NUL 的内容应被拒绝，实际：%v", err)
	}
	big := make([]byte, 2048)
	if _, _, err := m.SaveText("big.txt", big, 0); err != ErrTooLarge {
		t.Fatalf("超过编辑上限应被拒绝，实际：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin.dat")); !os.IsNotExist(err) {
		t.Fatal("被拒绝的保存不应留下文件")
	}
}

func TestSaveTextCreatesNewFileButNotDeletedOne(t *testing.T) {
	m, root := newTestManager(t, nil)
	if _, _, err := m.SaveText("new.txt", []byte("hello"), 0); err != nil {
		t.Fatalf("新建应成功：%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(data) != "hello" {
		t.Fatalf("新建内容不正确：%s", data)
	}
	// 客户端打开时文件还在，保存前被删了：不能悄悄写回来
	_, mt, _ := m.StatDetail("new.txt")
	os.Remove(filepath.Join(root, "new.txt"))
	if _, _, err := m.SaveText("new.txt", []byte("again"), mt); err != ErrConflict {
		t.Fatalf("文件已被删除时应返回冲突，实际：%v", err)
	}
}

func TestStatDetailNanoResolution(t *testing.T) {
	m, root := newTestManager(t, nil)
	os.WriteFile(filepath.Join(root, "t.txt"), []byte("x"), 0o644)
	_, ns1, err := m.StatDetail("t.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ns1 < 1e18 {
		t.Fatalf("修改时间精度不足，疑似仍是秒级：%d", ns1)
	}
}

func TestLooksBinary(t *testing.T) {
	if LooksBinary([]byte("纯文本\n第二行")) {
		t.Fatal("文本被误判为二进制")
	}
	if !LooksBinary([]byte("abc\x00def")) {
		t.Fatal("含 NUL 的内容应判为二进制")
	}
	// 只看前 8KB：NUL 在后面不算
	tail := append(make([]byte, 9000), 0)
	for i := range tail[:9000] {
		tail[i] = 'a'
	}
	if LooksBinary(tail) {
		t.Fatal("8KB 之后的 NUL 不应影响判断")
	}
}

func TestRelFromAbs(t *testing.T) {
	m, _ := newTestManager(t, nil)
	real := m.Root() // New 里已 EvalSymlinks，用它构造期望值

	if err := os.MkdirAll(filepath.Join(real, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		abs  string
		want string
		ok   bool
	}{
		{real, "", true},                                 // root 本身 → 空路径
		{filepath.Join(real, "a"), "a", true},            //
		{filepath.Join(real, "a", "b"), "a/b", true},     // 多级
		{filepath.Join(real, "a", "..", "a"), "a", true}, // 中间带 ..
		{filepath.Join(real, ".."), "", false},           // 父目录在 root 外
		{"", "", false},                                  // 空输入
		{"/etc/passwd", "", false},                       // 完全无关
	}
	for _, c := range cases {
		got, ok := m.RelFromAbs(c.abs)
		if ok != c.ok || got != c.want {
			t.Fatalf("RelFromAbs(%q) = (%q, %v)，期望 (%q, %v)", c.abs, got, ok, c.want, c.ok)
		}
	}
}

func TestRelFromAbsFollowsSymlink(t *testing.T) {
	m, _ := newTestManager(t, nil)
	real := m.Root()

	// 外部路径经符号链接指进 root：应当认得出来（用户 HOME 常是软链）
	outside := t.TempDir()
	into := filepath.Join(outside, "into-root")
	if err := os.Symlink(real, into); err != nil {
		t.Skip("当前系统不允许创建符号链接")
	}
	if rel, ok := m.RelFromAbs(into); !ok || rel != "" {
		t.Fatalf("指向 root 的软链应判定在 root 内，得到 (%q, %v)", rel, ok)
	}

	// root 内的软链指向外部：必须判定为 root 之外，与 resolve 的安全语义保持一致
	escaping := filepath.Join(real, "escape")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Skip("当前系统不允许创建符号链接")
	}
	if rel, ok := m.RelFromAbs(escaping); ok {
		t.Fatalf("越界软链应判定在 root 外，得到 (%q, %v)", rel, ok)
	}
}
