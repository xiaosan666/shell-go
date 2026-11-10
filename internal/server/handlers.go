package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	"shellgo/internal/audit"
	"shellgo/internal/auth"
	"shellgo/internal/terminal"
)

// Version 由 main 注入。
var Version = "dev"

func (s *Server) handleHandshake(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.hsLimiter.allow(ip) {
		writePlainError(w, &plainError{"握手请求过于频繁", http.StatusTooManyRequests})
		return
	}
	cs, err := s.newSession()
	if err != nil {
		s.log.Error("生成 SM2 密钥失败", "err", err)
		writePlainError(w, &plainError{"服务端密钥生成失败", http.StatusInternalServerError})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sid":     cs.id,
		"pub":     cs.kp.PublicHex(),
		"ts":      time.Now().Unix(),
		"alg":     "SM2/SM4-CBC/SM3-HMAC",
		"version": Version,
	})
}

type loginReq struct {
	U string `json:"u"`
	P string `json:"p"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	c, err := s.openLogin(w, r)
	if err != nil {
		if errors.Is(err, errAbort) {
			return
		}
		writePlainError(w, err)
		return
	}
	var req loginReq
	if !c.bind(&req) {
		return
	}
	sess, err := s.auth.Login(req.U, req.P, ip)
	if err != nil {
		s.audit.Log(audit.Event{Event: "login_fail", User: req.U, IP: ip, Detail: err.Error()})
		switch {
		case errors.Is(err, auth.ErrLocked):
			c.fail(codeBusiness, "登录失败次数过多，请稍后再试")
		case errors.Is(err, auth.ErrTooMany):
			c.fail(codeBusiness, "在线会话数已达上限，请联系管理员")
		default:
			c.fail(codeBusiness, "用户名或密码错误")
		}
		return
	}
	c.cs.user = sess.User
	c.cs.markUsed()
	c.user = sess.User
	s.audit.Log(audit.Event{Event: "login", User: sess.User, IP: ip})

	tc := s.cfg.Terminal
	c.ok(map[string]any{
		"token":     sess.Token,
		"expire":    time.Now().Add(s.cfg.Security.SessionTTL.D()).Unix(),
		"idle":      int(s.cfg.Security.IdleTimeout.D().Seconds()),
		"user":      sess.User,
		"caps":      sess.Caps,
		"transport": tc.Transport,
		"pollMs":    tc.PollMs,
		"title":     tc.Title,
		"maxTerms":  caps01(sess.Caps.MaxTerms, s.cfg.Security.MaxTerms),
	})
}

// caps01 取账号上限，未设置（0）时用全局值。
func caps01(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

func (s *Server) handleSession(c *call) {
	if c.user == "" {
		c.write(codeNoAuth, "登录已过期，请重新登录", nil)
		return
	}
	c.ok(map[string]any{
		"user":      c.user,
		"caps":      c.caps,
		"transport": s.cfg.Terminal.Transport,
		"pollMs":    s.cfg.Terminal.PollMs,
		"maxTerms":  caps01(c.caps.MaxTerms, s.cfg.Security.MaxTerms),
		"version":   Version,
	})
}

func (s *Server) handleLogout(c *call) {
	s.auth.Logout(c.p.Token)
	s.audit.Log(audit.Event{Event: "logout", User: c.user, IP: c.ip})
	c.ok(nil)
}

func (s *Server) handleSysInfo(c *call) {
	host, _ := os.Hostname()
	cur := ""
	if u, err := user.Current(); err == nil {
		cur = u.Username
	}
	c.ok(map[string]any{
		"host":    host,
		"os":      runtime.GOOS + "/" + runtime.GOARCH,
		"process": cur,
		"uptime":  int(time.Since(s.start).Seconds()),
		"terms":   s.terms.Count(),
		"online":  s.auth.Online(),
		"version": Version,
		"now":     time.Now().Unix(),
	})
}

// ---------- 终端 ----------

type termReq struct {
	Term   string `json:"term"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
	Cursor uint64 `json:"cursor"`
	Wait   int    `json:"wait"`
	Max    int    `json:"max"`
	Data   string `json:"data"`
	Title  string `json:"title"`
}

func (s *Server) handleTermCreate(c *call) {
	if !c.need(c.caps.Shell, "当前账号没有终端权限，请联系管理员") {
		return
	}
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, err := s.terms.Create(c.user, req.Cols, req.Rows, c.caps.MaxTerms)
	if err != nil {
		if errors.Is(err, terminal.ErrTooMany) {
			c.fail(codeBusiness, "终端数量已达上限，请先关闭不用的窗口")
			return
		}
		s.log.Error("创建终端失败", "err", err)
		c.fail(codeInternal, "创建终端失败："+err.Error())
		return
	}
	s.audit.Log(audit.Event{Event: "term_open", User: c.user, IP: c.ip, Term: t.ID})
	tc := s.cfg.Terminal
	snap, cursor := t.Snapshot(tc.SnapshotMax)
	c.ok(map[string]any{
		"id":       t.ID,
		"title":    "",
		"snapshot": base64.StdEncoding.EncodeToString(snap),
		"cursor":   cursor,
	})
}

func (s *Server) handleTermList(c *call) {
	c.ok(map[string]any{"terms": s.terms.List(c.user)})
}

func (s *Server) handleTermClose(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	if err := s.terms.Close(c.user, req.Term); err != nil {
		c.fail(codeBusiness, "终端不存在或已关闭")
		return
	}
	s.audit.Log(audit.Event{Event: "term_close", User: c.user, IP: c.ip, Term: req.Term})
	c.ok(nil)
}

func (s *Server) handleTermRename(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	title := strings.TrimSpace(req.Title)
	if len([]rune(title)) > 40 {
		title = string([]rune(title)[:40])
	}
	if title == "" {
		title = s.cfg.Terminal.Title
	}
	t.Rename(title)
	c.ok(nil)
}

func (s *Server) handleTermResize(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		return
	}
	t.Resize(req.Cols, req.Rows)
	c.ok(nil)
}

func (s *Server) handleTermInput(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		c.fail(codeBadReq, "输入数据非法")
		return
	}
	if err := t.Write(data); err != nil {
		c.fail(codeBusiness, "终端已关闭")
		return
	}
	c.ok(nil)
}

func (s *Server) handleTermPoll(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	wait := req.Wait
	if wait <= 0 || wait > 1500 {
		wait = 600
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	for {
		data, next, reset := t.ReadFrom(req.Cursor, 256*1024)
		if len(data) > 0 || t.Closed() || time.Now().After(deadline) {
			c.ok(map[string]any{
				"cursor": next,
				"data":   base64.StdEncoding.EncodeToString(data),
				"reset":  reset,
				"closed": t.Closed(),
			})
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *Server) handleTermSnapshot(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	max := req.Max
	if max <= 0 || max > s.cfg.Terminal.SnapshotMax*4 {
		max = s.cfg.Terminal.SnapshotMax
	}
	data, cursor := t.Snapshot(max)
	c.ok(map[string]any{
		"data":   base64.StdEncoding.EncodeToString(data),
		"cursor": cursor,
		"closed": t.Closed(),
	})
}

// handleTermCwd 返回终端 shell 当前所在的目录，供「打开文件管理时定位到这里」使用。
// inside 表示该目录在文件管理的 root 之内（是才给出 rel），否则前端会退回上次浏览的位置。
func (s *Server) handleTermCwd(c *call) {
	if !c.need(c.caps.FilesRead, "当前账号没有文件浏览权限") {
		return
	}
	var req termReq
	if !c.bind(&req) {
		return
	}
	t, ok := s.terms.Get(c.user, req.Term)
	if !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	abs, err := t.Cwd()
	if err != nil {
		c.fail(codeBusiness, "无法读取终端目录")
		return
	}
	resp := map[string]any{"abs": abs, "inside": false, "rel": ""}
	if rel, ok := s.files.RelFromAbs(abs); ok {
		resp["inside"] = true
		resp["rel"] = rel
	}
	c.ok(resp)
}

func (s *Server) handleTermTicket(c *call) {
	var req termReq
	if !c.bind(&req) {
		return
	}
	if _, ok := s.terms.Get(c.user, req.Term); !ok {
		c.fail(codeBusiness, "终端不存在")
		return
	}
	tk, err := s.issueTicket(c, req.Term)
	if err != nil {
		c.fail(codeInternal, "票据签发失败")
		return
	}
	c.ok(map[string]any{"ticket": tk})
}
