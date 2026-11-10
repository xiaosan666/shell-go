package terminal

import (
	"sync"
	"time"

	"shellgo/internal/config"
)

type Manager struct {
	mu       sync.Mutex
	terms    map[string]*Term
	tc       *config.Terminal
	rc       config.Record
	maxTerms int
	idle     time.Duration
}

func NewManager(tc *config.Terminal, rc config.Record, maxTerms int) *Manager {
	return &Manager{
		terms:    map[string]*Term{},
		tc:       tc,
		rc:       rc,
		maxTerms: maxTerms,
		idle:     tc.IdleClose.D(),
	}
}

// Create 新建终端。limit 是该账号的终端数上限，<=0 时退回全局上限——
// 不同角色的账号可以有各自的并发窗口数，全局值只作兜底。
func (m *Manager) Create(owner string, cols, rows uint16, limit int) (*Term, error) {
	max := limit
	if max <= 0 {
		max = m.maxTerms
	}
	m.mu.Lock()
	if max > 0 && m.countLocked(owner) >= max {
		m.mu.Unlock()
		return nil, ErrTooMany
	}
	m.mu.Unlock()

	t, err := newTerm(m.tc, m.rc, owner, cols, rows, m.remove)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.terms[t.ID] = t
	m.mu.Unlock()
	return t, nil
}

func (m *Manager) countLocked(owner string) int {
	n := 0
	for _, t := range m.terms {
		if t.Owner == owner && !t.Closed() {
			n++
		}
	}
	return n
}

func (m *Manager) Get(owner, id string) (*Term, bool) {
	m.mu.Lock()
	t, ok := m.terms[id]
	m.mu.Unlock()
	if !ok || t.Owner != owner {
		return nil, false
	}
	return t, true
}

func (m *Manager) List(owner string) []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.terms))
	for _, t := range m.terms {
		if t.Owner == owner {
			out = append(out, t.Info())
		}
	}
	return out
}

func (m *Manager) Close(owner, id string) error {
	t, ok := m.Get(owner, id)
	if !ok {
		return ErrNoTerm
	}
	t.Close()
	return nil
}

func (m *Manager) remove(t *Term) {
	m.mu.Lock()
	if cur, ok := m.terms[t.ID]; ok && cur == t {
		delete(m.terms, t.ID)
	}
	m.mu.Unlock()
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Term, 0, len(m.terms))
	for _, t := range m.terms {
		all = append(all, t)
	}
	m.mu.Unlock()
	for _, t := range all {
		t.Close()
	}
}

// Sweep 关闭长时间无人附着且空闲的终端。
func (m *Manager) Sweep() {
	if m.idle <= 0 {
		return
	}
	m.mu.Lock()
	var stale []*Term
	for _, t := range m.terms {
		if t.Subscribers() == 0 && t.IdleFor() > m.idle {
			stale = append(stale, t)
		}
	}
	m.mu.Unlock()
	for _, t := range stale {
		t.Close()
	}
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.terms)
}
