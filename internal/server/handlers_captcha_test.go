package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shellgo/internal/audit"
	"shellgo/internal/auth"
	"shellgo/internal/config"
	"shellgo/internal/crypto"
	"shellgo/internal/terminal"
)

func newTestServerWith(t *testing.T, tweak func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Users = []config.User{{Username: "admin", Password: "test1234"}}
	if tweak != nil {
		tweak(cfg)
	}
	am, err := auth.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tm := terminal.NewManager(&cfg.Terminal, cfg.Record, cfg.Security.MaxTerms)
	al := audit.New("", false)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, am, tm, nil, al, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, path, &buf))
	return w
}

// 下发的验证码必须带 cid 和两张 data URI 图片，且不含答案。
func TestCaptchaIssueHTTP(t *testing.T) {
	s := newTestServerWith(t, nil)
	w := post(t, s.handleCaptcha, "/api/captcha", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var ch struct {
		CID   string `json:"cid"`
		Bg    string `json:"bg"`
		Piece string `json:"piece"`
		W     int    `json:"w"`
		PY    int    `json:"py"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ch); err != nil {
		t.Fatal(err)
	}
	if ch.CID == "" || !strings.HasPrefix(ch.Bg, "data:image/png;base64,") ||
		!strings.HasPrefix(ch.Piece, "data:image/png;base64,") {
		t.Fatalf("验证码内容不完整：%+v", ch)
	}
	// 答案绝不能出现在响应里
	if strings.Contains(w.Body.String(), `"x"`) {
		t.Fatal("响应不应包含缺口坐标")
	}
}

// 位置不对要拒绝，且不能把验证码留着给人反复试。
func TestCaptchaVerifyRejectsWrongOffset(t *testing.T) {
	s := newTestServerWith(t, nil)
	w := post(t, s.handleCaptcha, "/api/captcha", nil)
	var ch struct {
		CID string `json:"cid"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ch)

	v := post(t, s.handleCaptchaVerify, "/api/captcha/verify", map[string]any{"cid": ch.CID, "x": 300})
	if v.Code != http.StatusBadRequest {
		t.Fatalf("错位应被拒绝，实际 %d：%s", v.Code, v.Body.String())
	}
	// 同一张图再用一次也是无效的
	v2 := post(t, s.handleCaptchaVerify, "/api/captcha/verify", map[string]any{"cid": ch.CID, "x": 300})
	if v2.Code != http.StatusBadRequest {
		t.Fatalf("作废的验证码应被拒绝，实际 %d", v2.Code)
	}
}

// 关闭开关时不发图，直接告诉前端跳过。
func TestCaptchaDisabledByConfig(t *testing.T) {
	s := newTestServerWith(t, func(cfg *config.Config) {
		cfg.Security.LoginCaptcha = config.OptBool{Set: true, Val: false}
	})
	w := post(t, s.handleCaptcha, "/api/captcha", nil)
	if !strings.Contains(w.Body.String(), `"disabled":true`) {
		t.Fatalf("关闭时应返回 disabled，实际：%s", w.Body.String())
	}
}

// 没有验证票据就不该走到密码校验，否则爆破不受滑块限制。
func TestLoginRequiresCaptchaTicket(t *testing.T) {
	s := newTestServerWith(t, nil)
	for _, ticket := range []string{"", "deadbeef"} {
		code, msg := loginWithTicket(t, s, ticket)
		if code != codeBusiness || !strings.Contains(msg, "滑动验证") {
			t.Fatalf("票据 %q 应被挡下，实际 code=%d msg=%s", ticket, code, msg)
		}
	}
}

// 关掉验证码后，原来的纯密码登录链路要照常可用。
func TestLoginWithoutCaptchaWhenDisabled(t *testing.T) {
	s := newTestServerWith(t, func(cfg *config.Config) {
		cfg.Security.LoginCaptcha = config.OptBool{Set: true, Val: false}
	})
	code, msg := loginWithTicket(t, s, "")
	if code != codeOK {
		t.Fatalf("关闭验证码后应能直接登录，实际 code=%d msg=%s", code, msg)
	}
}

// loginWithTicket 走完整的 SM2 换钥 + SM4 信封流程登录，返回业务码与提示。
func loginWithTicket(t *testing.T, s *Server, ticket string) (int, string) {
	t.Helper()
	cs, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	key := crypto.RandomKey()
	kHex, err := cs.kp.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"u": "admin", "p": "test1234", "ct": ticket})
	plain, err := json.Marshal(payload{
		TS:    time.Now().Unix(),
		Nonce: crypto.RandomHex(8),
		P:     inner,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := crypto.Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(loginEnvelope{SID: cs.id, K: kHex, Data: base64.StdEncoding.EncodeToString(sealed)})

	w := httptest.NewRecorder()
	s.handleLogin(w, httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body)))

	if w.Code != http.StatusOK {
		return -1, w.Body.String()
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是信封：%v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := crypto.Open(key, raw)
	if err != nil {
		t.Fatalf("响应解密失败：%v", err)
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(dec, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Code, resp.Msg
}
