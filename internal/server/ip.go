package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// parseIPRules 把配置里的 "1.2.3.4" 与 "10.0.0.0/8" 拆成单 IP 与网段两组规则。
func parseIPRules(raws []string) ([]net.IP, []*net.IPNet, error) {
	var ips []net.IP
	var nets []*net.IPNet
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			ips = append(ips, ip)
			continue
		}
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("%q 既不是合法 IP 也不是 CIDR", raw)
		}
		nets = append(nets, ipnet)
	}
	return ips, nets, nil
}

func ipInSet(ip net.IP, ips []net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, a := range ips {
		if a.Equal(ip) {
			return true
		}
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostOnly 只保留 IP 字面量：去掉端口与 IPv6 的方括号。
// 报头里的写法五花八门（"1.2.3.4:5678"、"[::1]:80"），这里统一归一化。
func hostOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	return strings.Trim(raw, "[]")
}

// clientIP 解析真实来源 IP。
//
// 默认只认 TCP 对端地址：X-Forwarded-For 是客户端可以随便写的头，
// 直接采信等于让任何人伪造白名单里的 IP。
// 只有当直连对端落在 security.trusted_proxies 里（也就是我们自己的反代）时，
// 才去看它写在报头里的来源地址。
func (s *Server) clientIP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !ipInSet(net.ParseIP(peer), s.proxyIPs, s.proxyNets) {
		return peer
	}

	// 从右往左扫：右边的值是代理自己追加的，伪造不了；
	// 越往左越可能是客户端自己塞进去的，不可信。
	var allTrusted string
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			h := hostOnly(parts[i])
			ip := net.ParseIP(h)
			if ip == nil {
				continue // 夹带垃圾值就跳过，不因此拒绝整个请求
			}
			if !ipInSet(ip, s.proxyIPs, s.proxyNets) {
				return h
			}
			allTrusted = h // 整条链都是代理，退回最左边那一跳
		}
	}
	if h := hostOnly(r.Header.Get("X-Real-IP")); h != "" && net.ParseIP(h) != nil {
		return h
	}
	if allTrusted != "" {
		return allTrusted
	}
	return peer
}
