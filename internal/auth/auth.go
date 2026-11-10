// Package auth 负责账号校验、会话令牌与登录失败锁定。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"shellgo/internal/config"
)

var (
	ErrBadCredential = errors.New("用户名或密码错误")
	ErrLocked        = errors.New("登录失败次数过多，请稍后再试")
	ErrNoSession     = errors.New("会话不存在或已过期")
	ErrTooMany       = errors.New("在线会话数已达上限")
)

type account struct {
	name string
	hash []byte
	caps config.Caps
}

type Session struct {
	Token    string
	User     string
	IP       string
	Caps     config.Caps
	Created  time.Time
	LastSeen time.Time
}

type failState struct {
	count int
	until time.Time
}

type Manager struct {
	mu       sync.Mutex
	accounts map[string]*account
	sessions map[string]*Session
	fails    map[string]*failState

	idleTimeout time.Duration
	ttl         time.Duration
	maxSessions int
	maxFail     int
	lockFor     time.Duration
}

func NewManager(cfg *config.Config) (*Manager, error) {
	m := &Manager{
		accounts:    map[string]*account{},
		sessions:    map[string]*Session{},
		fails:       map[string]*failState{},
		idleTimeout: cfg.Security.IdleTimeout.D(),
		ttl:         cfg.Security.SessionTTL.D(),
		maxSessions: cfg.Security.MaxSessions,
		maxFail:     cfg.Security.LoginMaxFail,
		lockFor:     cfg.Security.LoginLock.D(),
	}
	for _, u := range cfg.Users {
		var hash []byte
		if u.PasswordHash != "" {
			hash = []byte(u.PasswordHash)
			if _, err := bcrypt.Cost(hash); err != nil {
				return nil, fmt.Errorf("用户 %s 的 password_hash 不是合法 bcrypt 串：%w", u.Username, err)
			}
		} else {
			h, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
			if err != nil {
				return nil, err
			}
			hash = h
		}
		m.accounts[u.Username] = &account{
			name: u.Username,
			hash: hash,
			caps: u.CapsOf(cfg.Security.MaxTerms),
		}
	}
	return m, nil
}

// HasUser 判断账号是否存在（用于提前给出统一错误，避免用户名枚举差异）。
func (m *Manager) HasUser(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.accounts[name]
	return ok
}

// Caps 返回账号的能力集合；账号不存在时返回零值（全部无权限）。
func (m *Manager) Caps(name string) config.Caps {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ac, ok := m.accounts[name]; ok {
		return ac.caps
	}
	return config.Caps{}
}

func (m *Manager) locked(ip string) bool {
	f, ok := m.fails[ip]
	if !ok {
		return false
	}
	if time.Now().Before(f.until) {
		return true
	}
	return false
}

func (m *Manager) recordFail(ip string) {
	f, ok := m.fails[ip]
	if !ok {
		f = &failState{}
		m.fails[ip] = f
	}
	f.count++
	if m.maxFail > 0 && f.count >= m.maxFail {
		f.until = time.Now().Add(m.lockFor)
		f.count = 0
	}
}

// Login 校验账号密码并签发令牌。
func (m *Manager) Login(user, pass, ip string) (*Session, error) {
	m.mu.Lock()
	if m.locked(ip) {
		m.mu.Unlock()
		return nil, ErrLocked
	}
	ac, ok := m.accounts[user]
	if !ok {
		m.recordFail(ip)
		m.mu.Unlock()
		// 即使用户不存在也做一次 bcrypt，减少时序差异
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$CwTycUXWue0Thq9StjUM0uJ8DrsdOSTM9m0nJqQzZ5pWQ0m1FhEqe"), []byte(pass))
		return nil, ErrBadCredential
	}
	if err := bcrypt.CompareHashAndPassword(ac.hash, []byte(pass)); err != nil {
		m.recordFail(ip)
		m.mu.Unlock()
		return nil, ErrBadCredential
	}
	if m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		// 优先淘汰同一用户最久未活动的会话，避免用户关闭浏览器后长时间无法重新登录
		var oldest *Session
		for _, s := range m.sessions {
			if s.User != user {
				continue
			}
			if oldest == nil || s.LastSeen.Before(oldest.LastSeen) {
				oldest = s
			}
		}
		if oldest == nil {
			m.mu.Unlock()
			return nil, ErrTooMany
		}
		delete(m.sessions, oldest.Token)
	}
	delete(m.fails, ip)
	now := time.Now()
	s := &Session{
		Token:    randToken(),
		User:     user,
		IP:       ip,
		Caps:     ac.caps,
		Created:  now,
		LastSeen: now,
	}
	m.sessions[s.Token] = s
	m.mu.Unlock()
	return s, nil
}

// Validate 校验令牌并刷新活跃时间。
func (m *Manager) Validate(token string) (*Session, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok {
		return nil, ErrNoSession
	}
	now := time.Now()
	if now.Sub(s.LastSeen) > m.idleTimeout || now.Sub(s.Created) > m.ttl {
		delete(m.sessions, token)
		return nil, ErrNoSession
	}
	s.LastSeen = now
	return s, nil
}

func (m *Manager) Logout(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func (m *Manager) Online() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Sweep 定期清理过期会话与过期锁定记录。
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, s := range m.sessions {
		if now.Sub(s.LastSeen) > m.idleTimeout || now.Sub(s.Created) > m.ttl {
			delete(m.sessions, k)
		}
	}
	for k, f := range m.fails {
		if !f.until.IsZero() && now.After(f.until) {
			delete(m.fails, k)
		}
	}
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("系统随机数不可用：" + err.Error())
	}
	return hex.EncodeToString(b)
}

// ClientIP 取真实来源 IP（默认不信任 X-Forwarded-For，避免绕过白名单）。
func ClientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
