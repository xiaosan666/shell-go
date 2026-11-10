// Package files 提供受限目录内的文件浏览与读写。
//
// 所有对外接口都接收「相对 root 的路径」，内部统一做归一化与符号链接解析，
// 任何逃逸到 root 之外的请求都会被拒绝。
package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"shellgo/internal/config"
)

var (
	ErrDisabled  = errors.New("文件管理未启用")
	ErrForbidden = errors.New("路径超出允许范围")
	ErrIsDir     = errors.New("目标是目录")
	ErrNotDir    = errors.New("目标不是目录")
	ErrTooLarge  = errors.New("文件超过大小限制")
	ErrReadOnly  = errors.New("当前配置不允许写入")
	ErrNoDelete  = errors.New("当前配置不允许删除")
	ErrConflict  = errors.New("文件已被其他改动覆盖")
	ErrBinary    = errors.New("疑似二进制文件，不支持在线编辑")
)

// Entry 是目录列表中的一项。
type Entry struct {
	Name   string `json:"name"`
	Path   string `json:"path"` // 相对 root
	Dir    bool   `json:"dir"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	Mtime  int64  `json:"mtime"`
	Link   bool   `json:"link,omitempty"`
	Target string `json:"target,omitempty"`
}

type Manager struct {
	root      string
	maxUpload int64
	maxEdit   int64
	allowW    bool
	allowD    bool
	enabled   bool
}

func New(cfg config.Files) (*Manager, error) {
	if !cfg.Enabled {
		return &Manager{enabled: false}, nil
	}
	root, err := expand(cfg.Root)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("文件管理根目录不可用（%s）：%w", root, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("文件管理根目录不是目录：%s", root)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("解析根目录失败：%w", err)
	}
	maxEdit := int64(cfg.MaxEditKB) << 10
	if maxEdit <= 0 {
		maxEdit = 1024 << 10
	}
	return &Manager{
		root:      real,
		maxUpload: int64(cfg.MaxUploadMB) << 20,
		maxEdit:   maxEdit,
		allowW:    cfg.AllowWrite,
		allowD:    cfg.AllowDelete,
		enabled:   true,
	}, nil
}

func expand(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home + strings.TrimPrefix(p, "~")
	}
	return filepath.Abs(p)
}

func (m *Manager) Enabled() bool    { return m != nil && m.enabled }
func (m *Manager) Writable() bool   { return m.Enabled() && m.allowW }
func (m *Manager) Deletable() bool  { return m.Enabled() && m.allowD }
func (m *Manager) Root() string     { return m.root }
func (m *Manager) MaxUpload() int64 { return m.maxUpload }
func (m *Manager) MaxEdit() int64   { return m.maxEdit }

// RelFromAbs 把绝对路径映射成「相对 root」的路径，用于把终端的工作目录
// 翻译成文件管理里的位置；不在 root 之内时返回 false。
//
// 终端 cwd 常常带着符号链接（例如经 /data/app -> /real/app 进入），
// 先 EvalSymlinks 解析再比较，否则同一个目录会因为写法不同被判成「root 之外」。
// 目录已被删除时 EvalSymlinks 会失败，此时退回字面路径比较，让结果尽量可用。
func (m *Manager) RelFromAbs(abs string) (string, bool) {
	if !m.Enabled() || abs == "" {
		return "", false
	}
	p := filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	rel, err := filepath.Rel(m.root, p)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// cleanRel 把任意输入归一化成相对 root 的斜杠路径（"" 表示 root 本身）。
// 前导 / 被忽略，.. 在归一化时被消掉，因此不可能通过字符串层面逃逸。
func cleanRel(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrForbidden
	}
	rel = strings.ReplaceAll(rel, "\\", "/")
	p := path.Clean("/" + rel)
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		p = ""
	}
	return p, nil
}

// resolve 返回绝对路径，并保证结果在 root 之内（含符号链接解析）。
// wantExisting 为 true 时要求目标必须存在。
func (m *Manager) resolve(rel string, wantExisting bool) (string, string, error) {
	if !m.Enabled() {
		return "", "", ErrDisabled
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return "", "", err
	}
	abs := m.root
	if clean != "" {
		abs = filepath.Join(m.root, filepath.FromSlash(clean))
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if wantExisting {
			return "", clean, err
		}
		// 目标不存在：只校验父目录，确保新文件落在允许范围内。
		parent := filepath.Dir(abs)
		realParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", clean, err
		}
		if !within(m.root, realParent) {
			return "", clean, ErrForbidden
		}
		return filepath.Join(realParent, filepath.Base(abs)), clean, nil
	}
	if !within(m.root, real) {
		return "", clean, ErrForbidden
	}
	return real, clean, nil
}

func within(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(os.PathSeparator))
}

// List 返回目录内容。rel 为空表示 root。
func (m *Manager) List(rel string) (string, []Entry, error) {
	abs, clean, err := m.resolve(rel, true)
	if err != nil {
		return clean, nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return clean, nil, err
	}
	if !st.IsDir() {
		return clean, nil, ErrNotDir
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return clean, nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		e := Entry{Name: de.Name(), Path: joinRel(clean, de.Name()), Dir: de.IsDir()}
		if fi, err := de.Info(); err == nil {
			e.Size = fi.Size()
			e.Mode = fi.Mode().String()
			e.Mtime = fi.ModTime().Unix()
		}
		if de.Type()&os.ModeSymlink != 0 {
			e.Link = true
			if target, err := os.Readlink(filepath.Join(abs, de.Name())); err == nil {
				e.Target = target
			}
			// 符号链接指向目录时，按目录处理便于导航；指向 root 外则标灰。
			if fi, err := os.Stat(filepath.Join(abs, de.Name())); err == nil && fi.IsDir() {
				e.Dir = true
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return clean, out, nil
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (m *Manager) Stat(rel string) (Entry, error) {
	abs, clean, err := m.resolve(rel, true)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:  filepath.Base(abs),
		Path:  clean,
		Dir:   fi.IsDir(),
		Size:  fi.Size(),
		Mode:  fi.Mode().String(),
		Mtime: fi.ModTime().Unix(),
	}, nil
}

// OpenRead 打开文件供分块下载。
func (m *Manager) OpenRead(rel string) (*os.File, os.FileInfo, error) {
	abs, _, err := m.resolve(rel, true)
	if err != nil {
		return nil, nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() {
		return nil, nil, ErrIsDir
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, err
	}
	return f, fi, nil
}

// WriteChunk 追加写入分块。offset 必须等于当前文件长度（首块除外），
// 这样既避免稀疏文件，也能识别客户端乱序。
func (m *Manager) WriteChunk(rel string, offset, total int64, data []byte) (int64, error) {
	if !m.Writable() {
		return 0, ErrReadOnly
	}
	if total < 0 || total > m.maxUpload {
		return 0, ErrTooLarge
	}
	abs, _, err := m.resolve(rel, false)
	if err != nil {
		return 0, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if offset > 0 {
		fi, err := f.Stat()
		if err != nil {
			return 0, err
		}
		if fi.Size() != offset {
			return 0, fmt.Errorf("上传偏移不连续（期望 %d，实际 %d）", fi.Size(), offset)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset, err
		}
	}
	n, err := f.Write(data)
	if err != nil {
		return offset, err
	}
	return offset + int64(n), nil
}

func (m *Manager) Mkdir(rel string) error {
	if !m.Writable() {
		return ErrReadOnly
	}
	abs, _, err := m.resolve(rel, false)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

func (m *Manager) Rename(rel, name string) (string, error) {
	if !m.Writable() {
		return "", ErrReadOnly
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return "", errors.New("名称非法")
	}
	abs, clean, err := m.resolve(rel, true)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return "", ErrForbidden
	}
	dst := filepath.Join(filepath.Dir(abs), name)
	if !within(m.root, filepath.Dir(dst)) {
		return "", ErrForbidden
	}
	if err := os.Rename(abs, dst); err != nil {
		return "", err
	}
	dir := path.Dir(clean)
	if dir == "." {
		dir = ""
	}
	return joinRel(dir, name), nil
}

func (m *Manager) Remove(rel string, recursive bool) error {
	if !m.Deletable() {
		return ErrNoDelete
	}
	abs, clean, err := m.resolve(rel, true)
	if err != nil {
		return err
	}
	if clean == "" {
		return ErrForbidden
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if !recursive {
			return os.Remove(abs) // 非空目录会报错，正是我们要的行为
		}
		return os.RemoveAll(abs)
	}
	return os.Remove(abs)
}

// StatDetail 返回条目与纳秒级修改时间。
//
// 秒级不够用：同一秒内连续两次保存无法区分，在线编辑的冲突检测会漏判。
func (m *Manager) StatDetail(rel string) (Entry, int64, error) {
	abs, clean, err := m.resolve(rel, true)
	if err != nil {
		return Entry{}, 0, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Entry{}, 0, err
	}
	return Entry{
		Name:  filepath.Base(abs),
		Path:  clean,
		Dir:   fi.IsDir(),
		Size:  fi.Size(),
		Mode:  fi.Mode().String(),
		Mtime: fi.ModTime().Unix(),
	}, fi.ModTime().UnixNano(), nil
}

// LooksBinary 用「前 8KB 是否出现 NUL」判断二进制文件，不必读完整个文件。
// 在线编辑只对文本开放，避免把可执行文件当字符串写坏。
func LooksBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// SaveText 整体覆盖保存文本文件，用于网页内在线编辑。
//
// baseMtime 是客户端打开文件时拿到的修改时间（纳秒）。若文件在编辑期间被其他
// 进程改过，则返回 ErrConflict 而不是直接覆盖——这比「静默丢改动」好得多。
// baseMtime 为 0 表示客户端认为这是一次新建，此时若文件已存在同样视为冲突。
//
// 写入用「同目录临时文件 + rename」保证原子性：中途断电或进程被杀，
// 原文件要么是旧内容、要么是新内容，不会出现写了一半的残文件。
func (m *Manager) SaveText(rel string, data []byte, baseMtime int64) (int64, int64, error) {
	if !m.Writable() {
		return 0, 0, ErrReadOnly
	}
	if int64(len(data)) > m.maxEdit {
		return 0, 0, ErrTooLarge
	}
	if LooksBinary(data) {
		return 0, 0, ErrBinary
	}
	abs, clean, err := m.resolve(rel, false)
	if err != nil {
		return 0, 0, err
	}
	if clean == "" {
		return 0, 0, ErrForbidden
	}

	fi, err := os.Stat(abs)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, 0, err
	}
	if exists && fi.IsDir() {
		return 0, 0, ErrIsDir
	}
	switch {
	case exists && baseMtime <= 0:
		return 0, 0, ErrConflict
	case exists && fi.ModTime().UnixNano() != baseMtime:
		return 0, 0, ErrConflict
	case !exists && baseMtime > 0:
		// 打开后文件被删掉了，别再悄悄写回来
		return 0, 0, ErrConflict
	}

	perm := os.FileMode(0o644)
	if exists {
		perm = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".sgedit-*")
	if err != nil {
		return 0, 0, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return 0, 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, 0, err
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return 0, 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, 0, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return 0, 0, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return int64(len(data)), 0, err
	}
	return int64(len(data)), st.ModTime().UnixNano(), nil
}

// ReadChunk 从文件读取一段，返回数据与是否为末尾。
func ReadChunk(f *os.File, offset int64, size int) ([]byte, bool, error) {
	if size <= 0 || size > 1<<20 {
		size = 512 << 10
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false, err
	}
	buf = buf[:n]
	fi, serr := f.Stat()
	if serr != nil {
		return nil, false, serr
	}
	return buf, offset+int64(n) >= fi.Size(), nil
}
