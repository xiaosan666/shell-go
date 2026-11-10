package terminal

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"shellgo/internal/config"
)

// 录制文件为 JSONL：
//   第一行 {"v":1,"term":..,"user":..,"start":unix,"cols":..,"rows":..}
//   中间行 {"t":毫秒,"d":"base64(输出字节)"}
//   结束行 {"end":毫秒,"size":字节数}（正常关闭时写入，用于快速取时长）
// 只录输出：终端会把用户按键回显到输出流，因此回放与真实会话一致。

var recordIDRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}_[0-9a-f]{8,32}$`)

type RecordMeta struct {
	ID     string `json:"id"`
	Term   string `json:"term"`
	User   string `json:"user"`
	Start  int64  `json:"start"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
	Size   int64  `json:"size"`
	End    int64  `json:"end"` // 毫秒时长，0 表示录制尚未结束
	Events int    `json:"events,omitempty"`
}

type RecordEvent struct {
	T int64  `json:"t"`
	D string `json:"d"`
}

type Recorder struct {
	mu        sync.Mutex
	path      string
	f         *os.File
	w         *bufio.Writer
	written   int64
	max       int64
	meta      RecordMeta
	start     time.Time
	lastFlush time.Time
	stopped   bool
	closed    bool
}

// flushEvery 是录制落盘的间隔。太小会放大写系统调用，太大则「录制中」的会话
// 在列表里读不到内容，异常退出也会丢更多。
const flushEvery = 300 * time.Millisecond

func newRecorder(cfg config.Record, termID, user string, cols, rows uint16) (*Recorder, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	id := now.Format("20060102-150405") + "_" + termID
	if len(termID) > 16 {
		id = now.Format("20060102-150405") + "_" + termID[:16]
	}
	path := filepath.Join(cfg.Dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		path:      path,
		f:         f,
		w:         bufio.NewWriterSize(f, 32*1024),
		max:       int64(cfg.MaxMB) << 20,
		start:     now,
		lastFlush: now,
		meta: RecordMeta{
			ID:    id,
			Term:  termID,
			User:  user,
			Start: now.Unix(),
			Cols:  cols,
			Rows:  rows,
		},
	}
	head, _ := json.Marshal(r.meta)
	if _, err := r.w.Write(append(head, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	if err := r.w.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	go r.flushLoop()
	return r, nil
}

// flushLoop 定时把缓冲落盘。只在 WriteChunk 里按时间判断是不够的：
// 如果会话输出一次就安静下来（比如只打印了提示符），那一次写入时距创建不足
// flushEvery，之后不再有写入，数据会一直留在 bufio 里，直到 Close 才落盘，
// 表现为「录制中」的会话在列表里始终是 0 字节。
func (r *Recorder) flushLoop() {
	tk := time.NewTicker(flushEvery)
	defer tk.Stop()
	for range tk.C {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		if r.w.Buffered() > 0 {
			_ = r.w.Flush()
			r.lastFlush = time.Now()
		}
		r.mu.Unlock()
	}
}

// WriteChunk 记录一段终端输出。超过体积上限后自动停止记录，不再占用磁盘。
func (r *Recorder) WriteChunk(b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.closed {
		return
	}
	if r.max > 0 && r.written+int64(len(b)) > r.max {
		r.stopped = true
		_, _ = fmt.Fprintf(r.w, "{\"stop\":\"size-limit\"}\n")
		_ = r.w.Flush()
		return
	}
	line, err := json.Marshal(RecordEvent{
		T: time.Since(r.start).Milliseconds(),
		D: base64.StdEncoding.EncodeToString(b),
	})
	if err != nil {
		return
	}
	if _, err := r.w.Write(append(line, '\n')); err != nil {
		r.stopped = true
		return
	}
	r.written += int64(len(b))
	// 定时落盘：让「录制中」的会话也能被读到，同时把异常退出的损失限制在 300ms 内。
	if now := time.Now(); now.Sub(r.lastFlush) >= flushEvery {
		_ = r.w.Flush()
		r.lastFlush = now
	}
}

func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	tail, _ := json.Marshal(map[string]any{
		"end":  time.Since(r.start).Milliseconds(),
		"size": r.written,
	})
	_, _ = r.w.Write(append(tail, '\n'))
	_ = r.w.Flush()
	_ = r.f.Close()
	// 没有任何输出（打开就关掉的空标签）不留垃圾文件。
	if r.written == 0 && r.path != "" {
		_ = os.Remove(r.path)
	}
}

// ---------- 读取 ----------

func recordPath(dir, id string) (string, error) {
	if !recordIDRe.MatchString(id) || strings.ContainsAny(id, `/\`) {
		return "", errors.New("录制 ID 非法")
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// GetRecordMeta 只读元信息，用于权限判断与列表补全，
// 不必为此把整个录制文件读一遍。
func GetRecordMeta(dir, id string) (RecordMeta, error) {
	path, err := recordPath(dir, id)
	if err != nil {
		return RecordMeta{}, err
	}
	m, err := readMeta(path)
	if err != nil {
		return RecordMeta{}, err
	}
	m.ID = id
	if fi, err := os.Stat(path); err == nil {
		m.Size = fi.Size()
	}
	return m, nil
}

// ListRecords 返回录制列表（按开始时间倒序）。
func ListRecords(dir string) ([]RecordMeta, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]RecordMeta, 0, len(des))
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".jsonl")
		if !recordIDRe.MatchString(id) {
			continue
		}
		m, err := readMeta(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		m.ID = id
		if fi, err := de.Info(); err == nil {
			m.Size = fi.Size()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out, nil
}

func readMeta(path string) (RecordMeta, error) {
	var m RecordMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		return m, errors.New("空录制文件")
	}
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		return m, err
	}
	// 结束行在文件末尾，单独读取以取得时长，避免整文件扫描。
	if end, ok := tailEnd(path); ok {
		m.End = end
	}
	return m, nil
}

func tailEnd(path string) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return 0, false
	}
	n := int64(4096)
	if fi.Size() < n {
		n = fi.Size()
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, fi.Size()-n); err != nil && !errors.Is(err, io.EOF) {
		return 0, false
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	var tail struct {
		End *int64 `json:"end"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &tail); err != nil || tail.End == nil {
		return 0, false
	}
	return *tail.End, true
}

// LoadRecord 分页读取事件。from 为事件序号，count 为本次最多返回的条数。
func LoadRecord(dir, id string, from, count int) (RecordMeta, []RecordEvent, int, bool, error) {
	path, err := recordPath(dir, id)
	if err != nil {
		return RecordMeta{}, nil, 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return RecordMeta{}, nil, 0, false, err
	}
	defer f.Close()

	if count <= 0 || count > 1000 {
		count = 500
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return RecordMeta{}, nil, 0, false, errors.New("空录制文件")
	}
	var meta RecordMeta
	if err := json.Unmarshal(sc.Bytes(), &meta); err != nil {
		return RecordMeta{}, nil, 0, false, err
	}
	meta.ID = id
	if fi, err := f.Stat(); err == nil {
		meta.Size = fi.Size()
	}

	events := make([]RecordEvent, 0, count)
	idx := 0
	eof := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			T   *int64 `json:"t"`
			End *int64 `json:"end"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.End != nil {
			meta.End = *probe.End
			continue
		}
		if probe.T == nil {
			continue
		}
		if idx >= from {
			if len(events) >= count {
				eof = false
				break
			}
			var ev RecordEvent
			if err := json.Unmarshal(line, &ev); err == nil {
				events = append(events, ev)
			}
		}
		idx++
	}
	return meta, events, from + len(events), eof, sc.Err()
}

// CleanRecords 按保留天数与数量上限清理旧录制，返回删除数量。
func CleanRecords(dir string, keepDays, maxFiles int) int {
	list, err := ListRecords(dir)
	if err != nil || len(list) == 0 {
		return 0
	}
	removed := 0
	deadline := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
	live := make([]RecordMeta, 0, len(list))
	for _, m := range list {
		if keepDays > 0 && m.Start < deadline {
			if p, err := recordPath(dir, m.ID); err == nil {
				if os.Remove(p) == nil {
					removed++
				}
			}
			continue
		}
		live = append(live, m)
	}
	if maxFiles > 0 && len(live) > maxFiles {
		drop := len(live) - maxFiles
		for i := len(live) - 1; i >= 0 && drop > 0; i-- {
			if p, err := recordPath(dir, live[i].ID); err == nil {
				if os.Remove(p) == nil {
					removed++
					drop--
				}
			}
		}
	}
	return removed
}
