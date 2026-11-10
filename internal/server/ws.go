package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"shellgo/internal/audit"
	"shellgo/internal/crypto"
)

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 90 * time.Second
	wsPingPeriod = 25 * time.Second
	wsMaxMessage = 1 << 20
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     checkSameOrigin,
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// wsFrame 是加密后的控制帧。c 表示游标，x/y 表示列/行。
type wsFrame struct {
	T string `json:"t"`
	D string `json:"d,omitempty"`
	C uint64 `json:"c,omitempty"`
	X uint16 `json:"x,omitempty"`
	Y uint16 `json:"y,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	tk := s.takeTicket(r.URL.Query().Get("ticket"))
	if tk == nil {
		http.Error(w, "invalid ticket", http.StatusUnauthorized)
		return
	}
	cs := s.getSession(tk.sid)
	if cs == nil || len(cs.key) == 0 {
		http.Error(w, "session expired", http.StatusUnauthorized)
		return
	}
	t, ok := s.terms.Get(tk.user, tk.termID)
	if !ok {
		http.Error(w, "terminal not found", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub, snap, cursor, err := t.Attach(s.cfg.Terminal.SnapshotMax)
	if err != nil {
		return
	}
	defer t.Detach(sub)

	ip := clientIP(r)
	s.audit.Log(audit.Event{Event: "ws_open", User: tk.user, IP: ip, Term: t.ID})
	defer s.audit.Log(audit.Event{Event: "ws_close", User: tk.user, IP: ip, Term: t.ID})

	pongCh := make(chan uint64, 4)

	var once sync.Once
	stop := func(code int, reason string) {
		once.Do(func() {
			if code != 0 {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(code, reason), time.Now().Add(wsWriteWait))
			}
			_ = conn.Close()
		})
	}

	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer ticker.Stop()
		writeFrame := func(f wsFrame) error {
			plain, err := json.Marshal(f)
			if err != nil {
				return err
			}
			sealed, err := crypto.Seal(cs.key, plain)
			if err != nil {
				return err
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			return conn.WriteMessage(websocket.BinaryMessage, sealed)
		}
		if err := writeFrame(wsFrame{T: "s", D: base64.StdEncoding.EncodeToString(snap), C: cursor}); err != nil {
			stop(0, "")
			return
		}
		for {
			select {
			case chunk, ok := <-sub.Chan():
				if !ok {
					_ = writeFrame(wsFrame{T: "e"})
					stop(0, "")
					return
				}
				if sub.Overflow() {
					stop(websocket.CloseTryAgainLater, "overflow")
					return
				}
				if err := writeFrame(wsFrame{T: "o", D: base64.StdEncoding.EncodeToString(chunk)}); err != nil {
					stop(0, "")
					return
				}
			case seq := <-pongCh:
				if err := writeFrame(wsFrame{T: "q", C: seq}); err != nil {
					stop(0, "")
					return
				}
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
					stop(0, "")
					return
				}
				if err := writeFrame(wsFrame{T: "p"}); err != nil {
					stop(0, "")
					return
				}
			}
		}
	}()

	conn.SetReadLimit(wsMaxMessage)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			stop(0, "")
			return
		}
		plain, err := crypto.Open(cs.key, msg)
		if err != nil {
			continue
		}
		var f wsFrame
		if err := json.Unmarshal(plain, &f); err != nil {
			continue
		}
		switch f.T {
		case "i":
			if data, err := base64.StdEncoding.DecodeString(f.D); err == nil {
				_ = t.Write(data)
			}
		case "r":
			t.Resize(f.X, f.Y)
		case "p":
			select {
			case pongCh <- f.C:
			default:
			}
		}
	}
}
