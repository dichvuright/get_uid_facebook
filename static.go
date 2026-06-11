package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

// apiPublicBase: URL gốc để trình duyệt gọi API (tránh nhầm cổng 8080 proxy với LISTEN API).
func apiPublicBase(listen string, reqHost string) string {
	host := strings.TrimSpace(reqHost)
	if host != "" && !strings.Contains(host, ":") {
		host = host + listen
	}
	if host != "" {
		if strings.HasPrefix(host, ":") {
			return "http://127.0.0.1" + host
		}
		return "http://" + host
	}
	if strings.HasPrefix(listen, ":") {
		return "http://127.0.0.1" + listen
	}
	return "http://" + listen
}

func serveIndex(w http.ResponseWriter, r *http.Request, listen string) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Trang chủ chưa sẵn sàng", http.StatusInternalServerError)
		return
	}
	base := apiPublicBase(listen, r.Host)
	baseJS, _ := json.Marshal(base)
	html := strings.Replace(string(data), "__INJECT_API_BASE__", string(baseJS), 1)
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write([]byte(html))
}
