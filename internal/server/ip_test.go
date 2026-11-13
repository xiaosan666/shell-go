package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newIPServer(t *testing.T, whitelist, proxies []string) *Server {
	t.Helper()
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ips, nets, err := parseIPRules(whitelist)
	if err != nil {
		t.Fatalf("解析白名单失败：%v", err)
	}
	s.allowIPs, s.whitelist = ips, nets
	pips, pnets, err := parseIPRules(proxies)
	if err != nil {
		t.Fatalf("解析代理列表失败：%v", err)
	}
	s.proxyIPs, s.proxyNets = pips, pnets
	return s
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		peer    string
		proxies []string
		xff     string
		xreal   string
		want    string
	}{
		{name: "直连不采信 XFF", peer: "203.0.113.9:5000", xff: "10.9.9.9", want: "203.0.113.9"},
		{name: "可信代理取 XFF 单值", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32"}, xff: "203.0.113.9", want: "203.0.113.9"},
		{name: "取最右侧不可信地址", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32"}, xff: "1.1.1.1, 203.0.113.9", want: "203.0.113.9"},
		{name: "过滤链中间的代理节点", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32", "10.0.0.0/8"}, xff: "203.0.113.9, 10.0.0.5", want: "203.0.113.9"},
		{name: "带端口的条目", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32"}, xff: "203.0.113.9:53124", want: "203.0.113.9"},
		{name: "垃圾值跳过", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32"}, xff: "not-an-ip, 203.0.113.9", want: "203.0.113.9"},
		{name: "无 XFF 时用 X-Real-IP", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32"}, xreal: "198.51.100.7", want: "198.51.100.7"},
		{name: "XFF 全是代理退回最左", peer: "127.0.0.1:5000", proxies: []string{"127.0.0.1/32", "10.0.0.0/8"}, xff: "10.1.1.1, 10.0.0.5", want: "10.1.1.1"},
		{name: "IPv6 方括号与端口", peer: "[::1]:5000", proxies: []string{"::1/128"}, xff: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "对端不可信时忽略所有报头", peer: "198.51.100.20:5000", proxies: []string{"127.0.0.1/32"}, xff: "203.0.113.9", xreal: "203.0.113.9", want: "198.51.100.20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newIPServer(t, nil, tc.proxies)
			r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
			r.RemoteAddr = tc.peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xreal != "" {
				r.Header.Set("X-Real-IP", tc.xreal)
			}
			if got := s.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 白名单要按解析出来的真实 IP 判定，而不是代理地址。
func TestWhitelistBehindProxy(t *testing.T) {
	s := newIPServer(t, []string{"203.0.113.9"}, []string{"127.0.0.1/32"})
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name string
		xff  string
		want int
	}{
		{name: "真实 IP 在白名单内", xff: "203.0.113.9", want: http.StatusOK},
		{name: "真实 IP 不在白名单", xff: "198.51.100.1", want: http.StatusForbidden},
		{name: "左侧伪造不影响判定", xff: "203.0.113.9, 198.51.100.1", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "127.0.0.1:5000"
			r.Header.Set("X-Forwarded-For", tc.xff)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d", w.Code, tc.want)
			}
		})
	}
}
