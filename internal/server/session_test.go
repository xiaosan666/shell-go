package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shellgo/internal/audit"
	"shellgo/internal/auth"
	"shellgo/internal/config"
	"shellgo/internal/crypto"
	"shellgo/internal/terminal"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Users = []config.User{{Username: "admin", Password: "test1234"}}
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

// 已登录使用中的会话不应被「5 分钟未使用」清理掉，否则用户会用到一半掉登录。
func TestSweepKeepsUsedSession(t *testing.T) {
	s := newTestServer(t)

	used, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	used.markUsed()
	used.created = time.Now().Add(-10 * time.Minute)

	fresh, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	fresh.created = time.Now().Add(-6 * time.Minute) // 未登录且超过 5 分钟

	s.Sweep()

	if s.getSession(used.id) == nil {
		t.Fatal("使用中的会话被误删")
	}
	if s.getSession(fresh.id) != nil {
		t.Fatal("长期未登录的握手会话应被清理")
	}
}

func TestNonceReplayRejected(t *testing.T) {
	s := newTestServer(t)
	cs, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	if !cs.acceptNonce("abc", 300) {
		t.Fatal("首次 nonce 应被接受")
	}
	if cs.acceptNonce("abc", 300) {
		t.Fatal("重复 nonce 应被拒绝")
	}
	if cs.acceptNonce("", 300) {
		t.Fatal("空 nonce 应被拒绝")
	}
}

// 回归：openLogin 在验密码之前就把会话密钥写进了 cs，
// 所以「握手 + 输错密码」也能拿到能解密的通道。如果 openCall 允许空令牌，
// 未登录就能调文件/终端接口。这里把「必须有令牌」钉住。
func TestOpenCallRequiresToken(t *testing.T) {
	s := newTestServer(t)
	cs, err := s.newSession()
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, crypto.KeyLen)
	for i := range key {
		key[i] = byte(i*7 + 1)
	}
	cs.key = key

	callWith := func(token string) (int, string) {
		t.Helper()
		plain, _ := json.Marshal(payload{TS: time.Now().Unix(), Nonce: crypto.RandomHex(8), Token: token})
		sealed, err := crypto.Seal(key, plain)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(envelope{SID: cs.id, Data: base64.StdEncoding.EncodeToString(sealed)})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/file/list", bytes.NewReader(body))
		if _, err := s.openCall(w, req); !errors.Is(err, errAbort) {
			t.Fatalf("应中断请求，实际 err=%v", err)
		}
		var env envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("响应不是信封：%v", err)
		}
		raw, _ := base64.StdEncoding.DecodeString(env.Data)
		dec, err := crypto.Open(key, raw)
		if err != nil {
			t.Fatalf("响应解密失败：%v", err)
		}
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(dec, &resp)
		return resp.Code, resp.Msg
	}

	if code, msg := callWith(""); code != codeNoAuth {
		t.Fatalf("空令牌应返回未登录，实际 code=%d msg=%s", code, msg)
	}
	if code, msg := callWith("deadbeef"); code != codeNoAuth {
		t.Fatalf("伪造令牌应返回未登录，实际 code=%d msg=%s", code, msg)
	}

	// 正常登录后应当放行，并且带上账号能力
	sess, err := s.auth.Login("admin", "test1234", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(payload{TS: time.Now().Unix(), Nonce: crypto.RandomHex(8), Token: sess.Token})
	sealed, _ := crypto.Seal(key, plain)
	body, _ := json.Marshal(envelope{SID: cs.id, Data: base64.StdEncoding.EncodeToString(sealed)})
	req := httptest.NewRequest(http.MethodPost, "/api/file/list", bytes.NewReader(body))
	c, err := s.openCall(httptest.NewRecorder(), req)
	if err != nil {
		t.Fatalf("合法令牌不应被拒：%v", err)
	}
	if c.user != "admin" || !c.caps.Shell {
		t.Fatalf("应带上用户与能力，实际 user=%q caps=%+v", c.user, c.caps)
	}
}
