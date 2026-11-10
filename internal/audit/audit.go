// Package audit 以 JSONL 形式记录关键操作，便于事后追溯。
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const rotateSize = 20 << 20 // 20MB

type Event struct {
	Time   string `json:"time"`
	Event  string `json:"event"`
	User   string `json:"user,omitempty"`
	IP     string `json:"ip,omitempty"`
	Term   string `json:"term,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Logger struct {
	mu      sync.Mutex
	f       *os.File
	path    string
	size    int64
	enabled bool
}

func New(path string, enabled bool) *Logger {
	l := &Logger{path: path, enabled: enabled}
	if !enabled || path == "" {
		return l
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.enabled = false
		return l
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.enabled = false
		return l
	}
	st, _ := f.Stat()
	l.f = f
	if st != nil {
		l.size = st.Size()
	}
	return l
}

func (l *Logger) Enabled() bool { return l.enabled && l.f != nil }

func (l *Logger) Log(e Event) {
	if !l.Enabled() {
		return
	}
	e.Time = time.Now().Format("2006-01-02 15:04:05.000")
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.f.Write(b)
	if err == nil {
		l.size += int64(n)
	}
	if l.size > rotateSize {
		l.rotateLocked()
	}
}

func (l *Logger) rotateLocked() {
	_ = l.f.Close()
	_ = os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.f = nil
		l.enabled = false
		return
	}
	l.f = f
	l.size = 0
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
