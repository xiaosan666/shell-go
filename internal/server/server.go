package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"shellgo/internal/audit"
	"shellgo/internal/auth"
	"shellgo/internal/config"
	"shellgo/internal/crypto"
	"shellgo/internal/files"
	"shellgo/internal/terminal"
)

const (
	codeOK       = 0
	codeBadReq   = 1
	codeNoAuth   = 2
	codeInternal = 3
	codeBusiness = 4

	maxBodyBytes = 4 << 20
	tsWindow     = 300 // 秒
)

var errNoSession = errors.New("会话已失效")

type Server struct {
	cfg    *config.Config
	auth   *auth.Manager
	terms  *terminal.Manager
	files  *files.Manager
	audit  *audit.Logger
	log    *slog.Logger
	assets fs.FS
	start  time.Time

	mu       sync.RWMutex
	sessions map[string]*cryptoSession
	tickets  map[string]*ticket

	whitelist []*net.IPNet
	allowIPs  []net.IP

	hsLimiter *rateLimiter
}

type cryptoSession struct {
	id      string
	key     []byte
	kp      *crypto.KeyPair
	created time.Time
	used    bool
	user    string

	mu     sync.Mutex
	nonces map[string]int64
}

type ticket struct {
	user   string
	sid    string
	termID string
	exp    time.Time
}

func New(cfg *config.Config, am *auth.Manager, tm *terminal.Manager, fm *files.Manager, al *audit.Logger, log *slog.Logger, assets fs.FS) (*Server, error) {
	if fm == nil {
		fm, _ = files.New(config.Files{Enabled: false})
	}
	s := &Server{
		cfg:       cfg,
		auth:      am,
		terms:     tm,
		files:     fm,
		audit:     al,
		log:       log,
		assets:    assets,
		start:     time.Now(),
		sessions:  map[string]*cryptoSession{},
		tickets:   map[string]*ticket{},
		hsLimiter: newRateLimiter(60, time.Minute),
	}
	for _, raw := range cfg.Security.IPWhitelist {
		if ip := net.ParseIP(raw); ip != nil {
			s.allowIPs = append(s.allowIPs, ip)
			continue
		}
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("非法白名单项 %q", raw)
		}
		s.whitelist = append(s.whitelist, ipnet)
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 公开接口（握手）。
	mux.HandleFunc("POST /api/handshake", s.handleHandshake)
	mux.HandleFunc("POST /api/login", s.handleLogin)

	// 需要登录的接口。
	mux.HandleFunc("POST /api/session", s.wrap(s.handleSession))
	mux.HandleFunc("POST /api/logout", s.wrap(s.handleLogout))
	mux.HandleFunc("POST /api/sysinfo", s.wrap(s.handleSysInfo))
	mux.HandleFunc("POST /api/term/list", s.wrap(s.handleTermList))
	mux.HandleFunc("POST /api/term/create", s.wrap(s.handleTermCreate))
	mux.HandleFunc("POST /api/term/close", s.wrap(s.handleTermClose))
	mux.HandleFunc("POST /api/term/rename", s.wrap(s.handleTermRename))
	mux.HandleFunc("POST /api/term/resize", s.wrap(s.handleTermResize))
	mux.HandleFunc("POST /api/term/input", s.wrap(s.handleTermInput))
	mux.HandleFunc("POST /api/term/poll", s.wrap(s.handleTermPoll))
	mux.HandleFunc("POST /api/term/ticket", s.wrap(s.handleTermTicket))
	mux.HandleFunc("POST /api/term/snapshot", s.wrap(s.handleTermSnapshot))
	mux.HandleFunc("POST /api/term/cwd", s.wrap(s.handleTermCwd))

	// 文件管理（路径限制在 files.root 内）。
	mux.HandleFunc("POST /api/file/list", s.wrap(s.handleFileList))
	mux.HandleFunc("POST /api/file/read", s.wrap(s.handleFileRead))
	mux.HandleFunc("POST /api/file/write", s.wrap(s.handleFileWrite))
	mux.HandleFunc("POST /api/file/mkdir", s.wrap(s.handleFileMkdir))
	mux.HandleFunc("POST /api/file/rename", s.wrap(s.handleFileRename))
	mux.HandleFunc("POST /api/file/remove", s.wrap(s.handleFileRemove))
	mux.HandleFunc("POST /api/file/stat", s.wrap(s.handleFileStat))
	mux.HandleFunc("POST /api/file/save", s.wrap(s.handleFileSave))

	// 会话录制回放。
	mux.HandleFunc("POST /api/record/list", s.wrap(s.handleRecordList))
	mux.HandleFunc("POST /api/record/get", s.wrap(s.handleRecordGet))
	mux.HandleFunc("POST /api/record/delete", s.wrap(s.handleRecordDelete))

	// WebSocket 升级协议本身要求 GET，票据由上面的 POST 接口一次性签发。
	mux.HandleFunc("GET /ws", s.handleWS)

	mux.Handle("/", s.staticHandler())

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !s.ipAllowed(ip) {
			s.log.Warn("拒绝非白名单访问", "ip", ip, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self' ws: wss:; font-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) ipAllowed(ipStr string) bool {
	if len(s.whitelist) == 0 && len(s.allowIPs) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, a := range s.allowIPs {
		if a.Equal(ip) {
			return true
		}
	}
	for _, n := range s.whitelist {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------- 信封读写 ----------

type envelope struct {
	SID  string `json:"sid"`
	Data string `json:"data"`
}

type payload struct {
	TS    int64           `json:"ts"`
	Nonce string          `json:"nonce"`
	Token string          `json:"token"`
	P     json.RawMessage `json:"p"`
}

type call struct {
	s    *Server
	w    http.ResponseWriter
	r    *http.Request
	cs   *cryptoSession
	p    *payload
	user string
	caps config.Caps
	ip   string
}

// need 检查一项能力，不足时直接回包并返回 false，让调用方一行 return。
// 拒绝同时落审计：越权尝试本身是要留痕的事件。
func (c *call) need(ok bool, msg string) bool {
	if ok {
		return true
	}
	c.s.audit.Log(audit.Event{Event: "denied", User: c.user, IP: c.ip, Detail: msg})
	c.fail(codeBusiness, msg)
	return false
}

func (c *call) bind(v any) bool {
	if len(c.p.P) == 0 || string(c.p.P) == "null" {
		return true
	}
	if err := json.Unmarshal(c.p.P, v); err != nil {
		c.fail(codeBadReq, "参数格式错误")
		return false
	}
	return true
}

func (c *call) ok(data any) {
	c.write(codeOK, "", data)
}

func (c *call) fail(code int, msg string) {
	c.write(code, msg, nil)
}

func (c *call) write(code int, msg string, data any) {
	body := map[string]any{"code": code, "msg": msg, "data": data}
	plain, err := json.Marshal(body)
	if err != nil {
		http.Error(c.w, "internal error", http.StatusInternalServerError)
		return
	}
	sealed, err := crypto.Seal(c.cs.key, plain)
	if err != nil {
		http.Error(c.w, "internal error", http.StatusInternalServerError)
		return
	}
	c.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(c.w).Encode(envelope{SID: c.cs.id, Data: base64.StdEncoding.EncodeToString(sealed)})
}

func (s *Server) wrap(h func(*call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.openCall(w, r)
		if err != nil {
			if errors.Is(err, errAbort) {
				return
			}
			writePlainError(w, err)
			return
		}
		h(c)
	}
}

type plainError struct {
	msg  string
	code int
}

func (e *plainError) Error() string { return e.msg }

func writePlainError(w http.ResponseWriter, err error) {
	var pe *plainError
	if errors.As(err, &pe) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(pe.code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": pe.msg})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

type loginEnvelope struct {
	SID  string `json:"sid"`
	K    string `json:"k"` // SM2(会话密钥)，04||C1C3C2 的 hex
	Data string `json:"data"`
}

// openLogin 处理登录请求：先用 SM2 解出会话密钥，再用该密钥解开载荷。
func (s *Server) openLogin(w http.ResponseWriter, r *http.Request) (*call, error) {
	var env loginEnvelope
	if err := decodeBody(r, &env); err != nil {
		return nil, &plainError{"请求体解析失败", http.StatusBadRequest}
	}
	cs := s.getSession(env.SID)
	if cs == nil {
		return nil, &plainError{"会话已失效，请刷新页面", http.StatusUnauthorized}
	}
	key, err := cs.kp.Decrypt(env.K)
	if err != nil || len(key) != crypto.KeyLen {
		return nil, &plainError{"密钥协商失败", http.StatusBadRequest}
	}
	cs.key = key

	sealed, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, &plainError{"请求体不是合法 base64", http.StatusBadRequest}
	}
	plain, err := crypto.Open(key, sealed)
	if err != nil {
		return nil, &plainError{"报文解密失败", http.StatusBadRequest}
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, &plainError{"报文内容解析失败", http.StatusBadRequest}
	}
	if math.Abs(float64(time.Now().Unix()-p.TS)) > tsWindow {
		return nil, &plainError{"请求已过期，请检查服务器与浏览器时间", http.StatusBadRequest}
	}
	if !cs.acceptNonce(p.Nonce, tsWindow) {
		return nil, &plainError{"重复请求被拒绝", http.StatusBadRequest}
	}
	return &call{s: s, w: w, r: r, cs: cs, p: &p, ip: clientIP(r)}, nil
}

func (s *Server) openCall(w http.ResponseWriter, r *http.Request) (*call, error) {
	var env envelope
	if err := decodeBody(r, &env); err != nil {
		return nil, &plainError{"请求体解析失败", http.StatusBadRequest}
	}
	cs := s.getSession(env.SID)
	if cs == nil {
		return nil, &plainError{"会话已失效，请刷新页面", http.StatusUnauthorized}
	}
	sealed, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, &plainError{"请求体不是合法 base64", http.StatusBadRequest}
	}
	plain, err := crypto.Open(cs.key, sealed)
	if err != nil {
		return nil, &plainError{"报文解密失败", http.StatusBadRequest}
	}
	var p payload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, &plainError{"报文内容解析失败", http.StatusBadRequest}
	}
	if math.Abs(float64(time.Now().Unix()-p.TS)) > tsWindow {
		return nil, &plainError{"请求已过期，请检查服务器与浏览器时间", http.StatusBadRequest}
	}
	if !cs.acceptNonce(p.Nonce, tsWindow) {
		return nil, &plainError{"重复请求被拒绝", http.StatusBadRequest}
	}
	c := &call{s: s, w: w, r: r, cs: cs, p: &p, ip: clientIP(r)}
	// 令牌必须存在。openLogin 在验密码之前就把会话密钥写进了 cs，
	// 所以「握手 + 故意输错密码」也能拿到一个可解密的通道；
	// 如果这里允许空令牌，未登录就能调用文件、终端等接口。
	if p.Token == "" {
		c.write(codeNoAuth, "登录已过期，请重新登录", nil)
		return nil, errAbort
	}
	sess, err := s.auth.Validate(p.Token)
	if err != nil {
		c.write(codeNoAuth, "登录已过期，请重新登录", nil)
		return nil, errAbort
	}
	c.user = sess.User
	c.caps = sess.Caps
	cs.markUsed()
	return c, nil
}

var errAbort = errors.New("abort")

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// ---------- 会话与票据 ----------

func (s *Server) newSession() (*cryptoSession, error) {
	kp, err := crypto.NewKeyPair()
	if err != nil {
		return nil, err
	}
	cs := &cryptoSession{
		id:      crypto.RandomHex(16),
		key:     nil,
		kp:      kp,
		created: time.Now(),
		nonces:  map[string]int64{},
	}
	s.mu.Lock()
	s.sessions[cs.id] = cs
	s.mu.Unlock()
	return cs, nil
}

func (s *Server) getSession(id string) *cryptoSession {
	if id == "" {
		return nil
	}
	s.mu.RLock()
	cs := s.sessions[id]
	s.mu.RUnlock()
	return cs
}

// markUsed 标记会话已进入正常使用阶段，避免被 5 分钟未登录清理误删。
func (cs *cryptoSession) markUsed() {
	cs.mu.Lock()
	cs.used = true
	cs.mu.Unlock()
}

func (cs *cryptoSession) acceptNonce(n string, window int64) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	now := time.Now().Unix()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for k, t := range cs.nonces {
		if now-t > window {
			delete(cs.nonces, k)
		}
	}
	if _, ok := cs.nonces[n]; ok {
		return false
	}
	cs.nonces[n] = now
	return true
}

func (s *Server) issueTicket(c *call, termID string) (string, error) {
	tk := crypto.RandomHex(24)
	s.mu.Lock()
	s.tickets[tk] = &ticket{user: c.user, sid: c.cs.id, termID: termID, exp: time.Now().Add(30 * time.Second)}
	s.mu.Unlock()
	return tk, nil
}

func (s *Server) takeTicket(id string) *ticket {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[id]
	if !ok {
		return nil
	}
	delete(s.tickets, id)
	if time.Now().After(t.exp) {
		return nil
	}
	return t
}

// Sweep 周期性清理过期会话与票据。
func (s *Server) Sweep() {
	now := time.Now()
	s.mu.Lock()
	for id, cs := range s.sessions {
		cs.mu.Lock()
		used := cs.used
		created := cs.created
		cs.mu.Unlock()
		if now.Sub(created) > 12*time.Hour || (!used && now.Sub(created) > 5*time.Minute) {
			delete(s.sessions, id)
		}
	}
	for id, t := range s.tickets {
		if now.After(t.exp) {
			delete(s.tickets, id)
		}
	}
	s.mu.Unlock()
}

// ---------- 静态资源 ----------

func (s *Server) staticHandler() http.Handler {
	if s.assets == nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(s.assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		f, err := s.assets.Open(p)
		if err != nil {
			// 单页应用：未知路径一律回 index.html
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			http.ServeFileFS(w, r2, s.assets, "index.html")
			return
		}
		_ = f.Close()
		if strings.HasSuffix(p, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------- 简单限流 ----------

type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(max int, w time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: w, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.hits[key]
	keep := list[:0]
	for _, t := range list {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}
