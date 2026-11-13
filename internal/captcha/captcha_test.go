package captcha

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image/png"
	"testing"
	"time"
)

// Issue 下发的图片必须是合法 PNG，且拼图块的尺寸与声明一致。
func TestIssueRendersImages(t *testing.T) {
	m := New()
	ch, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID == "" {
		t.Fatal("缺少 cid")
	}
	if ch.W != ImageW || ch.H != ImageH {
		t.Fatalf("尺寸不符：%dx%d", ch.W, ch.H)
	}
	if ch.PW <= 0 || ch.PH <= 0 {
		t.Fatalf("拼图块尺寸非法：%dx%d", ch.PW, ch.PH)
	}
	if ch.PY < 0 || ch.PY+ch.PH > ch.H {
		t.Fatalf("拼图块纵向越界：py=%d ph=%d h=%d", ch.PY, ch.PH, ch.H)
	}

	bg, err := decodeDataURI(ch.Bg)
	if err != nil {
		t.Fatalf("背景图解码失败：%v", err)
	}
	img, err := png.Decode(bytes.NewReader(bg))
	if err != nil {
		t.Fatalf("背景图不是 PNG：%v", err)
	}
	if img.Bounds().Dx() != ch.W || img.Bounds().Dy() != ch.H {
		t.Fatalf("背景图尺寸不符：%v", img.Bounds())
	}

	piece, err := decodeDataURI(ch.Piece)
	if err != nil {
		t.Fatalf("拼图块解码失败：%v", err)
	}
	pimg, err := png.Decode(bytes.NewReader(piece))
	if err != nil {
		t.Fatalf("拼图块不是 PNG：%v", err)
	}
	if pimg.Bounds().Dx() != ch.PW || pimg.Bounds().Dy() != ch.PH {
		t.Fatalf("拼图块尺寸不符：%v", pimg.Bounds())
	}
}

func TestVerifyAcceptsAlignedPosition(t *testing.T) {
	m := New()
	ch, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	answer := m.pending[ch.ID].x

	ticket, err := m.Verify(ch.ID, answer+3) // 容差内
	if err != nil {
		t.Fatalf("容差内的落点应通过：%v", err)
	}
	if ticket == "" {
		t.Fatal("应签发票据")
	}
	if err := m.Consume(ticket); err != nil {
		t.Fatalf("票据应可用一次：%v", err)
	}
	if err := m.Consume(ticket); !errors.Is(err, ErrTicket) {
		t.Fatalf("票据应只能用一次，实际 err=%v", err)
	}
}

func TestVerifyRejectsWrongPositionAndBurnsChallenge(t *testing.T) {
	m := New()
	ch, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	answer := m.pending[ch.ID].x

	if _, err := m.Verify(ch.ID, answer+Tolerance+5); !errors.Is(err, ErrMismatch) {
		t.Fatalf("超出容差应失败，实际 err=%v", err)
	}
	// 猜错一次后，同一张图不能再用——否则可以反复试位置。
	if _, err := m.Verify(ch.ID, answer); !errors.Is(err, ErrExpired) {
		t.Fatalf("失败后应作废该验证码，实际 err=%v", err)
	}
}

func TestVerifyExpired(t *testing.T) {
	m := New()
	ch, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	m.pending[ch.ID].exp = time.Now().Add(-time.Second)
	if _, err := m.Verify(ch.ID, m.pending[ch.ID].x); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期应失败，实际 err=%v", err)
	}
	if _, err := m.Verify("nope", 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("未知 id 应失败，实际 err=%v", err)
	}
}

func TestConsumeRejectsExpiredTicket(t *testing.T) {
	m := New()
	m.tickets["t1"] = time.Now().Add(-time.Minute)
	if err := m.Consume("t1"); !errors.Is(err, ErrTicket) {
		t.Fatalf("过期票据应失败，实际 err=%v", err)
	}
	if err := m.Consume(""); !errors.Is(err, ErrTicket) {
		t.Fatalf("空票据应失败，实际 err=%v", err)
	}
}

func TestSweepDropsStale(t *testing.T) {
	m := New()
	ch, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	m.pending[ch.ID].exp = time.Now().Add(-time.Second)
	m.tickets["t"] = time.Now().Add(-time.Second)
	m.Sweep()
	if len(m.pending) != 0 || len(m.tickets) != 0 {
		t.Fatalf("过期数据未清理：pending=%d tickets=%d", len(m.pending), len(m.tickets))
	}
}

// decodeDataURI 从 data:image/png;base64,... 里取出字节。
func decodeDataURI(uri string) ([]byte, error) {
	const p = "data:image/png;base64,"
	if len(uri) <= len(p) || uri[:len(p)] != p {
		return nil, errors.New("不是 PNG data URI")
	}
	return base64.StdEncoding.DecodeString(uri[len(p):])
}
