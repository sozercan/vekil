package server

import (
	"net"
	"net/http"
	"strings"
)

var forwardedMetricsHeaders = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Forwarded-Port",
	"X-Real-IP",
	"Via",
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemoteAddr(r.RemoteAddr) || hasForwardingHeaders(r.Header) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasForwardingHeaders(header http.Header) bool {
	for _, name := range forwardedMetricsHeaders {
		if strings.TrimSpace(header.Get(name)) != "" {
			return true
		}
	}
	return false
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return false
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if i := strings.LastIndex(host, "%"); i >= 0 {
		host = host[:i]
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
