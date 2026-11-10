package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"shellgo/internal/config"
)

var (
	ErrClosed  = errors.New("终端已关闭")
	ErrTooMany = errors.New("终端数量已达上限")
	ErrNoTerm  = errors.New("终端不存在")
)

// Sub 是一个输出订阅者（对应一个 WebSocket 连接）。
type Sub struct {
	ch       chan []byte
	overflow atomic.Bool
}

func newSub() *Sub { return &Sub{ch: make(chan []byte, 256)} }

// Chan 返回只读通道，关闭时由 Term 负责 close。
func (s *Sub) Chan() <-chan []byte { return s.ch }

// Overflow 表示该订阅者积压过多，客户端应重连并重新拉取快照。
func (s *Sub) Overflow() bool { return s.overflow.Load() }

type Term struct {
	ID    string
	Owner string

	mu         sync.Mutex
	title      string
	cmd        *exec.Cmd
	ptmx       *os.File
	ring       *Ring
	cols, rows uint16
	created    time.Time
	lastActive time.Time
	closed     bool
	subs       map[*Sub]struct{}
	rec        *Recorder
	onExit     func(*Term)
}

func newTermID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("系统随机数不可用：" + err.Error())
	}
	return hex.EncodeToString(b)
}

// start 启动 shell 进程并挂上伪终端。
func newTerm(tc *config.Terminal, rc config.Record, owner string, cols, rows uint16, onExit func(*Term)) (*Term, error) {
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 32
	}
	cmd := exec.Command(tc.Shell, tc.Args...)
	cmd.Env = buildEnv(tc.Env)
	if dir := expandHome(tc.Cwd); dir != "" {
		cmd.Dir = dir
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	// 录制失败不影响终端使用（磁盘满、权限不足等），只是没有回放记录。
	rec, _ := newRecorder(rc, newTermID(), owner, cols, rows)
	id := ""
	if rec != nil {
		id = rec.meta.Term
	}
	t := &Term{
		ID:         id,
		Owner:      owner,
		title:      "",
		cmd:        cmd,
		ptmx:       ptmx,
		ring:       NewRing(tc.Scrollback),
		cols:       cols,
		rows:       rows,
		created:    time.Now(),
		lastActive: time.Now(),
		subs:       map[*Sub]struct{}{},
		rec:        rec,
		onExit:     onExit,
	}
	if id == "" {
		t.ID = newTermID()
	}
	go t.readLoop()
	return t, nil
}

func buildEnv(extra map[string]string) []string {
	env := os.Environ()
	idx := map[string]int{}
	for i, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			idx[k] = i
		}
	}
	for k, v := range extra {
		if i, ok := idx[k]; ok {
			env[i] = k + "=" + v
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return home + strings.TrimPrefix(p, "~")
	}
	return p
}

func (t *Term) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			t.ring.Write(chunk)
			t.rec.WriteChunk(chunk)
			t.broadcast(chunk)
			t.touch()
		}
		if err != nil {
			break
		}
	}
	t.finish()
}

func (t *Term) broadcast(chunk []byte) {
	t.mu.Lock()
	subs := make([]*Sub, 0, len(t.subs))
	for s := range t.subs {
		subs = append(subs, s)
	}
	t.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- chunk:
		default:
			s.overflow.Store(true)
		}
	}
}

func (t *Term) touch() {
	t.mu.Lock()
	t.lastActive = time.Now()
	t.mu.Unlock()
}

func (t *Term) finish() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	for s := range t.subs {
		close(s.ch)
	}
	t.subs = map[*Sub]struct{}{}
	onExit := t.onExit
	t.mu.Unlock()

	_ = t.ptmx.Close()
	_ = t.cmd.Wait()
	t.rec.Close()
	if onExit != nil {
		onExit(t)
	}
}

// Write 把用户输入写入 shell。
func (t *Term) Write(p []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}
	_, err := t.ptmx.Write(p)
	t.touch()
	return err
}

// Resize 调整伪终端尺寸。
func (t *Term) Resize(cols, rows uint16) {
	// 拒绝异常尺寸：隐藏容器或布局未完成时浏览器可能上报极小值，
	// 会让 shell 按错误宽度折行，回放时出现碎裂的提示符。
	if cols < 20 || rows < 5 {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.cols, t.rows = cols, rows
	ptmx := t.ptmx
	t.mu.Unlock()
	_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	t.touch()
}

// Attach 注册一个订阅者，并返回当前快照与下一个偏移量。
func (t *Term) Attach(snapshotMax int) (*Sub, []byte, uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, nil, 0, ErrClosed
	}
	s := newSub()
	t.subs[s] = struct{}{}
	snap, next := t.ring.Tail(snapshotMax)
	t.lastActive = time.Now()
	return s, snap, next, nil
}

func (t *Term) Detach(s *Sub) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subs[s]; ok {
		delete(t.subs, s)
		close(s.ch)
	}
	t.lastActive = time.Now()
}

// Snapshot 返回最近的数据与偏移（轮询/重连使用）。
func (t *Term) Snapshot(max int) ([]byte, uint64) { return t.ring.Tail(max) }

// ReadFrom 按偏移读取（轮询模式）。
func (t *Term) ReadFrom(cursor uint64, max int) ([]byte, uint64, bool) {
	return t.ring.ReadFrom(cursor, max)
}

func (t *Term) Size() uint64 { return t.ring.Size() }

func (t *Term) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Cwd 返回登录 shell 当前的工作目录。
//
// Linux 下直接读 /proc/<pid>/cwd：shell 每次 cd，内核就更新这个链接，
// 比自己解析提示符或往 PTY 里塞 pwd 都可靠（后者会把命令写到用户屏幕上）。
// 它反映的是「这一层 shell 的目录」——在 shell 里再 ssh / docker exec 到别处，
// 拿到的仍是本机这一层的 cwd，嵌套环境的目录无从得知。
func (t *Term) Cwd() (string, error) {
	t.mu.Lock()
	closed, cmd := t.closed, t.cmd
	t.mu.Unlock()
	if closed || cmd == nil || cmd.Process == nil {
		return "", ErrClosed
	}
	if runtime.GOOS != "linux" {
		return "", errors.New("当前平台不支持读取终端工作目录")
	}
	p, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid))
	if err != nil {
		return "", err
	}
	return p, nil
}

func (t *Term) Subscribers() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subs)
}

func (t *Term) IdleFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.lastActive)
}

func (t *Term) Info() Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Info{
		ID:      t.ID,
		Title:   t.title,
		Cols:    t.cols,
		Rows:    t.rows,
		Created: t.created.Unix(),
		Closed:  t.closed,
	}
}

func (t *Term) Rename(title string) {
	t.mu.Lock()
	t.title = title
	t.mu.Unlock()
}

// Close 终止 shell 进程（先 SIGHUP，超时后 SIGKILL）。
func (t *Term) Close() {
	t.mu.Lock()
	closed := t.closed
	cmd := t.cmd
	t.mu.Unlock()
	if closed || cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// pty.Start 使用 Setsid，子进程为会话首进程，pgid == pid
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if t.Closed() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}()
}

type Info struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Cols    uint16 `json:"cols"`
	Rows    uint16 `json:"rows"`
	Created int64  `json:"created"`
	Closed  bool   `json:"closed"`
}
