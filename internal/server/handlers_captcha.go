package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"shellgo/internal/audit"
	"shellgo/internal/captcha"
)

// captchaOn 判断登录是否要求滑动验证。默认开启，只有显式配置 false 才关闭。
func (s *Server) captchaOn() bool {
	return s.cfg.Security.LoginCaptcha.Or(true)
}

// handleCaptcha 下发一张滑动验证码。
// 与其他登录前接口一样走明文 JSON：这里只有图片，答案留在服务端。
func (s *Server) handleCaptcha(w http.ResponseWriter, r *http.Request) {
	if !s.captchaOn() {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"disabled": true})
		return
	}
	ip := clientIP(r)
	if !s.capLimit.allow(ip) {
		writePlainError(w, &plainError{"验证码请求过于频繁，请稍后重试", http.StatusTooManyRequests})
		return
	}
	ch, err := s.puzzle.Issue()
	if err != nil {
		s.log.Error("生成验证码失败", "err", err)
		writePlainError(w, &plainError{"验证码生成失败，请刷新重试", http.StatusInternalServerError})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(ch)
}

type captchaVerifyReq struct {
	CID string `json:"cid"`
	X   int    `json:"x"` // 拖动结束后拼图块的横向位移（图片像素）
}

// handleCaptchaVerify 校验拖动结果，通过后签发一张登录用的一次性票据。
func (s *Server) handleCaptchaVerify(w http.ResponseWriter, r *http.Request) {
	if !s.captchaOn() {
		writePlainError(w, &plainError{"验证码已关闭", http.StatusBadRequest})
		return
	}
	ip := clientIP(r)
	if !s.capVerify.allow(ip) {
		writePlainError(w, &plainError{"验证过于频繁，请稍后重试", http.StatusTooManyRequests})
		return
	}
	var req captchaVerifyReq
	if err := decodeBody(r, &req); err != nil {
		writePlainError(w, &plainError{"请求体解析失败", http.StatusBadRequest})
		return
	}
	ticket, err := s.puzzle.Verify(req.CID, req.X)
	if err != nil {
		s.audit.Log(audit.Event{Event: "captcha_fail", IP: ip, Detail: err.Error()})
		msg := "拼图位置不正确，请重试"
		switch {
		case errors.Is(err, captcha.ErrExpired):
			msg = "验证码已过期，请刷新后重试"
		case errors.Is(err, captcha.ErrBusy):
			msg = "验证码服务繁忙，请稍后重试"
		}
		writePlainError(w, &plainError{msg, http.StatusBadRequest})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket": ticket})
}
