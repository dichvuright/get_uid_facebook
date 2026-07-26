package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ====== Cấu hình cố định ======
// Cookie FB + proxy m2proxy của anh, dùng thẳng không cần flag
const (
	defaultCookie    = "datr=EKAqahfNwJS_qvLnSAwQvmdH; sb=EKAqau1ZOj3GUm_XR16xEc0r; ps_l=1; ps_n=1; wd=612x945"
	defaultProxyUser = ""
	defaultListen    = ":8787"
	defaultProxyHost = ""
	defaultProxyTLS  = false // true = dùng HTTPS proxy (CONNECT qua TLS)
	defaultTimeout   = 25
	defaultRetries   = 2
)

// ====== Đọc file .env ======
// Định dạng: KEY=VALUE (mỗi dòng 1 cặp). Comment bắt đầu bằng #.
// KHÔNG cần thư viện ngoài. Hỗ trợ quoted value: KEY="giá trị có space"
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // .env không tồn tại thì dùng const mặc định, im lặng
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tách KEY=VALUE (chỉ split lần đầu)
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Bỏ quote bao ngoài nếu có
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		os.Setenv(key, val)
	}
}

// Helper lấy env, fallback về default
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// Helper parse bool từ env: chấp nhận true/1/yes/on (case-insensitive) -> true
// false/0/no/off -> false. Sai -> fallback.
func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "true", "1", "yes", "y", "on":
		return true
	case "false", "0", "no", "n", "off":
		return false
	}
	return fallback
}

// Proxy pool: giữ session cố định (không rotate), vì m2proxy tự xoay IP
// theo session-id (1 request = 1 IP mới từ pool IP của m2proxy).
type ProxyPool struct {
	mu       sync.Mutex
	host     string
	userPart string
	passPart string
	proxyURL *url.URL // cache proxy URL dùng chung
}

func NewProxyPool(host, fixedUser string, _ int) *ProxyPool {
	// Tách user:pass từ fixedUser (giữ nguyên session ID)
	var userPart, passPart string
	if i := strings.Index(fixedUser, ":"); i >= 0 {
		userPart = fixedUser[:i]
		passPart = fixedUser[i+1:]
	} else {
		userPart = fixedUser
	}
	return &ProxyPool{
		host:     host,
		userPart: userPart,
		passPart: passPart,
		proxyURL: &url.URL{
			Scheme: "http",
			User:   url.UserPassword(userPart, passPart),
			Host:   host,
		},
	}
}

// Lấy proxy URL – luôn trả cùng 1 URL (m2proxy tự xoay IP theo session-id)
func (p *ProxyPool) Next() *url.URL {
	return p.proxyURL
}

// URL trả về string (cho log / debug)
func (p *ProxyPool) String() string {
	return p.proxyURL.String()
}

// ====== Kết quả trả về ======
type Result struct {
	Status      string  `json:"status"`
	ID          string  `json:"id,omitempty"`
	Username    string  `json:"username,omitempty"`
	Name        string  `json:"name,omitempty"`
	URL         string  `json:"url,omitempty"`
	Author      string  `json:"author"`
	Message     string  `json:"message,omitempty"`
	TimeCheck   string  `json:"time_check,omitempty"`   // ISO 8601 giờ Việt Nam (UTC+7), vd: 2026-06-11T21:30:25+07:00
	TimeElapsed float64 `json:"time_elapsed,omitempty"` // Thời gian xử lý (giây, float)
}

func main() {
	// Load file .env (nếu có) - không bắt buộc, không có thì dùng const mặc định
	loadEnv(".env")

	// ====== Flags ======
	listen := flag.String("listen", getEnv("LISTEN", defaultListen), "Địa chỉ lắng nghe (vd: :8080)")
	proxyHost := flag.String("proxy-host", getEnv("PROXY_HOST", defaultProxyHost), "Host:port của proxy m2proxy")
	proxyUser := flag.String("proxy-user", getEnv("PROXY_USER", defaultProxyUser), "User:pass của proxy m2proxy (có thể kèm session-XXX)")
	proxyCount := flag.Int("proxy-pool", getEnvInt("PROXY_POOL", 200), "Số session proxy xoay vòng")
	// -no-proxy=true sẽ ghi đè USE_PROXY=false
	// -no-proxy không truyền -> đọc từ env USE_PROXY (mặc định true)
	noProxyFlag := flag.Bool("no-proxy", false, "Ghi đè env USE_PROXY. -no-proxy=true = tắt proxy")
	timeout := flag.Int("timeout", getEnvInt("TIMEOUT", defaultTimeout), "Timeout mỗi request (giây)")
	retries := flag.Int("retries", getEnvInt("RETRIES", defaultRetries), "Số lần retry khi lỗi")
	flag.Parse()

	// Cookie lấy từ env, fallback về const mặc định
	cookie := getEnv("FB_COOKIE", defaultCookie)

	// Quyết định dùng proxy hay không
	// Thứ tự ưu tiên:
	//   1. Nếu flag -no-proxy được set rõ ràng (true/false) -> dùng flag
	//   2. Nếu flag -no-proxy không set -> đọc từ env USE_PROXY (mặc định false)
	// Lưu ý: flag mặc định là false, nên cần check xem user có thực sự truyền flag hay không
	useProxy := getEnvBool("USE_PROXY", false)
	// Nếu flag -no-proxy được set rõ ràng thì ưu tiên
	if isFlagSet("no-proxy") {
		useProxy = !*noProxyFlag
	}

	var pool *ProxyPool
	if useProxy {
		pool = NewProxyPool(*proxyHost, *proxyUser, *proxyCount)
		fmt.Printf(">> Proxy RA NGOÀI: BẬT → %s (IP gateway do m2proxy cấp, VD 198.54.x.x — KHÔNG phải IP máy anh)\n", *proxyHost)
	} else {
		fmt.Println(">> Proxy RA NGOÀI: TẮT (gọi Facebook trực tiếp)")
	}
	fmt.Printf(">> API web (máy anh): http://127.0.0.1%s — khách gọi vào đây, không trùng server proxy\n", *listen)
	if strings.HasSuffix(*listen, ":8080") && useProxy {
		fmt.Println(">> Gợi ý: LISTEN=:8787 trong .env để đỡ nhầm với PROXY_HOST ...:8080")
	}

	proxyFallback := getEnvBool("PROXY_FALLBACK_DIRECT", true)
	if !useProxy {
		initFBDirectTransport(*timeout)
	}
	runServer(*listen, cookie, pool, proxyFallback, *timeout, *retries)
}

// isFlagSet kiểm tra flag có được truyền rõ ràng từ CLI hay không
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// ============================================================
// HEADER POOL - xoay vòng nhiều bộ Chrome thật để giảm bị FB block
// ============================================================

// Mỗi entry là 1 bộ (User-Agent, sec-ch-ua, sec-ch-ua-full-version-list,
// accept-language, viewport-width, color-scheme) khớp nhau – mô phỏng đúng trình duyệt
var headerProfiles = []headerProfile{
	// Chrome 132 - Windows
	{
		ua:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36",
		secChUA:         `"Not A(Brand";v="8", "Chromium";v="132", "Google Chrome";v="132"`,
		secChUAFull:     `"Not A(Brand";v="8.0.0.0", "Chromium";v="132.0.6834.197", "Google Chrome";v="132.0.6834.197"`,
		acceptLanguage:  "vi-VN,vi;q=0.9,fr-FR;q=0.8,fr;q=0.7,en-US;q=0.6,en;q=0.5",
		viewportWidth:   "612",
		platform:        `"Windows"`,
		platformVersion: `"19.0.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 131 - Windows
	{
		ua:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		secChUA:         `"Not A(Brand";v="8", "Chromium";v="131", "Google Chrome";v="131"`,
		secChUAFull:     `"Not A(Brand";v="8.0.0.0", "Chromium";v="131.0.6778.205", "Google Chrome";v="131.0.6778.205"`,
		acceptLanguage:  "en-US,en;q=0.9",
		viewportWidth:   "1920",
		platform:        `"Windows"`,
		platformVersion: `"19.0.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 130 - Windows
	{
		ua:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		secChUA:         `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		secChUAFull:     `"Chromium";v="130.0.6723.116", "Google Chrome";v="130.0.6723.116", "Not?A_Brand";v="99.0.0.0"`,
		acceptLanguage:  "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7",
		viewportWidth:   "1366",
		platform:        `"Windows"`,
		platformVersion: `"15.0.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 132 - Linux
	{
		ua:              "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36",
		secChUA:         `"Not A(Brand";v="8", "Chromium";v="132", "Google Chrome";v="132"`,
		secChUAFull:     `"Not A(Brand";v="8.0.0.0", "Chromium";v="132.0.6834.197", "Google Chrome";v="132.0.6834.197"`,
		acceptLanguage:  "en-US,en;q=0.9,vi;q=0.8",
		viewportWidth:   "1920",
		platform:        `"Linux"`,
		platformVersion: `"6.5.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 131 - macOS
	{
		ua:              "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		secChUA:         `"Not A(Brand";v="8", "Chromium";v="131", "Google Chrome";v="131"`,
		secChUAFull:     `"Not A(Brand";v="8.0.0.0", "Chromium";v="131.0.6778.205", "Google Chrome";v="131.0.6778.205"`,
		acceptLanguage:  "en-GB,en;q=0.9,en-US;q=0.8",
		viewportWidth:   "1440",
		platform:        `"macOS"`,
		platformVersion: `"14.6.1"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 130 - Windows (thấp hơn)
	{
		ua:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		secChUA:         `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		secChUAFull:     `"Chromium";v="130.0.6723.116", "Google Chrome";v="130.0.6723.116", "Not?A_Brand";v="99.0.0.0"`,
		acceptLanguage:  "vi,en;q=0.9,en-US;q=0.8",
		viewportWidth:   "1536",
		platform:        `"Windows"`,
		platformVersion: `"15.0.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 129 - Windows
	{
		ua:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
		secChUA:         `"Google Chrome";v="129", "Not=A?Brand";v="8", "Chromium";v="129"`,
		secChUAFull:     `"Google Chrome";v="129.0.6668.89", "Not=A?Brand";v="8.0.0.0", "Chromium";v="129.0.6668.89"`,
		acceptLanguage:  "fr-FR,fr;q=0.9,en-US;q=0.8,en;q=0.7,vi;q=0.6",
		viewportWidth:   "1680",
		platform:        `"Windows"`,
		platformVersion: `"10.0.0"`,
		uaMobile:        "?0",
		uaModel:         `""`,
		secGPC:          "1",
	},
	// Chrome 132 - Android mobile (ít dùng nhưng có thật)
	{
		ua:              "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Mobile Safari/537.36",
		secChUA:         `"Not A(Brand";v="8", "Chromium";v="132", "Google Chrome";v="132"`,
		secChUAFull:     `"Not A(Brand";v="8.0.0.0", "Chromium";v="132.0.6834.197", "Google Chrome";v="132.0.6834.197"`,
		acceptLanguage:  "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7",
		viewportWidth:   "412",
		platform:        `"Android"`,
		platformVersion: `"14"`,
		uaMobile:        "?1",
		uaModel:         `"Pixel 7"`,
		secGPC:          "1",
	},
}

type headerProfile struct {
	ua              string
	secChUA         string
	secChUAFull     string
	acceptLanguage  string
	viewportWidth   string
	platform        string
	platformVersion string
	uaMobile        string
	uaModel         string
	secGPC          string
}

var headerIdx int64 // atomic counter cho header xoay vòng

// Pool HTTP ra Facebook — tái dùng TLS/TCP (lần 2+ thường ~0.3–0.5s nhanh hơn lần 1)
var (
	fbDirectTransport *http.Transport
	fbTransportOnce   sync.Once
)

func initFBDirectTransport(timeoutSec int) {
	fbTransportOnce.Do(func() {
		dial := 6 * time.Second
		hdr := 8 * time.Second
		if timeoutSec > 0 && timeoutSec < 12 {
			hdr = time.Duration(timeoutSec) * time.Second
		}
		fbDirectTransport = &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			MaxConnsPerHost:       32,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    false,
			ForceAttemptHTTP2:     false,
			ResponseHeaderTimeout: hdr,
			DialContext: (&net.Dialer{
				Timeout:   dial,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ExpectContinueTimeout: 1 * time.Second,
		}
		// Warm TLS tới facebook.com (best-effort, không chặn start)
		go func() {
			c := &http.Client{Transport: fbDirectTransport, Timeout: 5 * time.Second}
			req, _ := http.NewRequest(http.MethodHead, "https://www.facebook.com/", nil)
			if req != nil {
				_, _ = c.Do(req)
			}
		}()
	})
}

// setHeaders xoay vòng theo index, mỗi request 1 bộ header Chrome khác nhau.
// referer="" nghĩa là đi thẳng (gõ URL) -> sec-fetch-site=none.
// Truyền referer khi đi từ link nội bộ -> sec-fetch-site=same-origin.
func setHeaders(req *http.Request, cookie, referer string) {
	idx := atomic.AddInt64(&headerIdx, 1) - 1
	p := headerProfiles[int(idx)%len(headerProfiles)]

	h := req.Header
	h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	h.Set("accept-language", p.acceptLanguage)
	// Không set Accept-Encoding: để Go tự thêm "gzip" (nếu DisableCompression=false)
	// và tự giải nén response. Nếu set thủ công "gzip, deflate" Facebook vẫn trả về
	// gzip nhưng một số trường hợp Go không decode -> nhận binary rác.
	h.Set("cache-control", "max-age=0")
	h.Set("cookie", cookie)
	h.Set("dpr", "1")
	h.Set("priority", "u=0, i")
	h.Set("sec-ch-prefers-color-scheme", "dark")
	h.Set("sec-ch-ua", p.secChUA)
	h.Set("sec-ch-ua-full-version-list", p.secChUAFull)
	h.Set("sec-ch-ua-mobile", p.uaMobile)
	h.Set("sec-ch-ua-model", p.uaModel)
	h.Set("sec-ch-ua-platform", p.platform)
	h.Set("sec-ch-ua-platform-version", p.platformVersion)
	h.Set("sec-fetch-dest", "document")
	h.Set("sec-fetch-mode", "navigate")
	if referer == "" {
		// Truy cập trực tiếp (gõ URL thanh address bar)
		h.Set("sec-fetch-site", "none")
	} else {
		h.Set("sec-fetch-site", "same-origin")
		h.Set("referer", referer)
	}
	h.Set("sec-fetch-user", "?1")
	h.Set("sec-gpc", p.secGPC)
	h.Set("upgrade-insecure-requests", "1")
	h.Set("user-agent", p.ua)
	h.Set("viewport-width", p.viewportWidth)
	// Header Chrome trên desktop thường gửi kèm
	h.Set("device-memory", "8")
	h.Set("downlink", "10")
	h.Set("ect", "4g")
	h.Set("rtt", "50")
	// Connection: keep-alive
	h.Set("Connection", "keep-alive")
}

func runServer(listen, cookie string, pool *ProxyPool, proxyFallbackDirect bool, timeout, retries int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/facebook", func(w http.ResponseWriter, r *http.Request) {
		handleFacebookAPI(w, r, cookie, pool, proxyFallbackDirect, timeout, retries)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveIndex(w, r, listen)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"proxy_outbound":  pool != nil,
			"proxy_host":      getEnv("PROXY_HOST", defaultProxyHost),
			"fallback_direct": proxyFallbackDirect,
		})
	})
	// Debug: gọi thẳng tới Facebook qua 3 host (www / mbasic / m) và trả về
	// status, final_url, độ dài html, cờ is_login, preview 600 ký tự đầu.
	// Dùng để xem tại sao 1 profile bị dính login / không tìm thấy UID.
	//
	// VD: curl 'http://127.0.0.1:8787/debug/raw?url=dichvuright.max'
	mux.HandleFunc("/debug/raw", func(w http.ResponseWriter, r *http.Request) {
		handleDebugRaw(w, r, cookie, pool, proxyFallbackDirect, timeout)
	})
	// Debug: xem header profile thứ n (0..7)
	mux.HandleFunc("/debug/header", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		n := 0
		if v := r.URL.Query().Get("n"); v != "" {
			fmt.Sscanf(v, "%d", &n)
		}
		if n < 0 {
			n = 0
		}
		if n >= len(headerProfiles) {
			n = n % len(headerProfiles)
		}
		p := headerProfiles[n]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"index": n,
			"total": len(headerProfiles),
			"profile": map[string]string{
				"user-agent":                  p.ua,
				"sec-ch-ua":                   p.secChUA,
				"sec-ch-ua-full-version-list": p.secChUAFull,
				"sec-ch-ua-mobile":            p.uaMobile,
				"sec-ch-ua-model":             p.uaModel,
				"sec-ch-ua-platform":          p.platform,
				"sec-ch-ua-platform-version":  p.platformVersion,
				"accept-language":             p.acceptLanguage,
				"viewport-width":              p.viewportWidth,
				"sec-gpc":                     p.secGPC,
			},
		})
	})

	fmt.Printf(">> Server đang lắng nghe tại %s\n", listen)
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println("Lỗi server:", err)
		os.Exit(1)
	}
}

// Handler chính của API
// handleDebugRaw: gọi Facebook qua 3 host (www -> mbasic -> m), trả về thông tin debug.
// Không parse UID, chỉ quan tâm status, finalURL, độ dài html, có phải login không,
// và preview HTML để soi.
func handleDebugRaw(w http.ResponseWriter, r *http.Request, cookie string, pool *ProxyPool, proxyFallbackDirect bool, timeout int) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "missing url param"})
		return
	}
	// Có thể truyền nguyên username (dichvuright.max) hoặc URL đầy đủ
	if !strings.Contains(target, "facebook.com") && !strings.HasPrefix(target, "http") {
		target = "https://www.facebook.com/" + target
	} else if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}

	hosts := []string{"www.facebook.com", "mbasic.facebook.com", "m.facebook.com"}
	type oneResult struct {
		Host      string `json:"host"`
		URL       string `json:"url"`
		FinalURL  string `json:"final_url"`
		Status    int    `json:"status"`
		HTMLLen   int    `json:"html_len"`
		IsLogin   bool   `json:"is_login_or_checkpoint"`
		Preview   string `json:"html_preview"`
		Err       string `json:"error,omitempty"`
		HeaderIdx int    `json:"header_profile_idx"`
	}
	results := make([]oneResult, 0, len(hosts))

	// Lấy proxy (nếu có)
	var proxyURL *url.URL
	if pool != nil {
		proxyURL = pool.Next()
	}
	for _, host := range hosts {
		urlStr := swapFacebookHost(target, host)
		uidIdx := atomic.AddInt64(&headerIdx, 1) - 1
		profileIdx := int(uidIdx) % len(headerProfiles)

		client := makeClient(proxyURL, timeout)
		req, errReq := http.NewRequest("GET", urlStr, nil)
		if errReq != nil {
			results = append(results, oneResult{Host: host, URL: urlStr, Err: errReq.Error(), HeaderIdx: profileIdx})
			continue
		}
		setHeaders(req, cookie, "")

		resp, errDo := client.Do(req)
		if errDo != nil {
			results = append(results, oneResult{Host: host, URL: urlStr, Err: errDo.Error(), HeaderIdx: profileIdx})
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		resp.Body.Close()

		finalURL := urlStr
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		html := string(body)
		results = append(results, oneResult{
			Host:      host,
			URL:       urlStr,
			FinalURL:  finalURL,
			Status:    resp.StatusCode,
			HTMLLen:   len(html),
			IsLogin:   isFacebookLoginOrCheckpoint(html, finalURL),
			Preview:   previewHTML(html, 800),
			HeaderIdx: profileIdx,
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"target":         target,
		"proxy_outbound": proxyURL != nil,
		"cookie_present": cookie != "",
		"results":        results,
		"time_check":     time.Now().Format(time.RFC3339),
	})
}

// previewHTML: cắt bớt tag/script, lấy N ký tự đầu để dễ nhìn
func previewHTML(html string, max int) string {
	// Bỏ <script>...</script> và <style>...</style> cho gọn
	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	s := reScript.ReplaceAllString(html, "")
	s = reStyle.ReplaceAllString(s, "")
	// Nén whitespace
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func handleFacebookAPI(w http.ResponseWriter, r *http.Request, cookie string, pool *ProxyPool, proxyFallbackDirect bool, timeout, retries int) {
	startTime := time.Now()

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Chỉ nhận GET/POST
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.WriteHeader(204)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(405)
		_ = json.NewEncoder(w).Encode(Result{
			Status: "error", Author: "DichVuRight", Message: "Method không hợp lệ, dùng GET hoặc POST.",
		})
		return
	}

	// Lấy url từ query hoặc form
	target := r.URL.Query().Get("url")
	if target == "" && r.Method == http.MethodPost {
		_ = r.ParseForm()
		target = r.FormValue("url")
	}

	// Hỗ trợ ?username=<chuỗi> – tự ghép thành link FB đầy đủ (chỉ dùng khi url rỗng)
	if target == "" {
		usernameRaw := r.URL.Query().Get("username")
		if usernameRaw == "" && r.Method == http.MethodPost {
			_ = r.ParseForm()
			usernameRaw = r.FormValue("username")
		}
		usernameRaw = strings.TrimSpace(strings.TrimPrefix(usernameRaw, "@"))
		if usernameRaw != "" {
			// Nếu user nhập ngắn gọn thì tự thêm scheme + host
			if !strings.HasPrefix(usernameRaw, "http://") && !strings.HasPrefix(usernameRaw, "https://") {
				target = "https://www.facebook.com/" + usernameRaw
			} else {
				target = usernameRaw
			}
		}
	}

	if target == "" {
		respond(w, 400, Result{
			Status: "error", Author: "DichVuRight", Message: "Thiếu tham số url hoặc username.",
		}, startTime)
		return
	}

	// Dùng cookie cố định từ config (không cho override qua query)
	reqCookie := cookie

	// Tách username từ link
	username, err := extractUsername(target)
	if err != nil {
		respond(w, 400, Result{
			Status: "error", Author: "DichVuRight", Message: err.Error(), URL: target,
		}, startTime)
		return
	}

	uid, finalURL, name, _, err := fetchUIDViaProxy(pool, reqCookie, target, username, proxyFallbackDirect, timeout, retries)
	if err != nil || uid == "" {
		msg := formatFetchError(err, pool != nil)
		if msg == "" && uid == "" {
			msg = "Không tìm thấy UID trong trang Facebook (profile ẩn hoặc link không hợp lệ)."
		}
		respond(w, 502, Result{
			Status: "error", Author: "DichVuRight",
			Username: username, Name: name, URL: finalURL,
			Message: msg,
		}, startTime)
		return
	}

	respUser := username
	if resolved := usernameFromResolvedURL(finalURL); resolved != "" {
		if looksLikeShareSlug(username) || resolved != username {
			respUser = displayUsername(resolved)
		}
	}
	cleanURL := canonicalProfileURL(finalURL, uid)
	if respUser == uid && strings.HasPrefix(usernameFromResolvedURL(finalURL), "id:") {
		// Profile chỉ có số — username hiển thị trùng id (không dùng token share)
		respUser = uid
	}

	respond(w, 200, Result{
		Status:   "success",
		ID:       uid,
		Username: respUser,
		Name:     name,
		URL:      cleanURL,
		Author:   "DichVuRight",
	}, startTime)
}

// respond ghi response, tự động gắn time_check (giờ VN UTC+7) + time_elapsed (giây)
func respond(w http.ResponseWriter, code int, r Result, startTime time.Time) {
	// Load TZ Việt Nam (UTC+7) - dùng fixed zone để tránh phụ thuộc hệ thống
	vnLoc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	if vnLoc == nil {
		// Fallback nếu tzdata không có sẵn (Windows thường thiếu) -> tự tính UTC+7
		vnLoc = time.FixedZone("ICT", 7*60*60)
	}
	now := time.Now().In(vnLoc)
	r.TimeCheck = now.Format(time.RFC3339)
	// time_elapsed tính bằng giây (float), 3 chữ số thập phân
	r.TimeElapsed = float64(time.Since(startTime).Microseconds()) / 1_000_000.0
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}

// Tách username từ mọi dạng link FB
// Chấp nhận:
//   - englunez
//   - @englunez
//   - https://www.facebook.com/englunez
//   - https://www.facebook.com/englunez?xxx=yyy
//   - https://www.facebook.com/profile.php?id=1000...
//   - https://fb.com/englunez
//   - https://m.facebook.com/englunez/posts/123...
//   - https://www.facebook.com/groups/xxx/user/1000...
//   - share links, /?id=
func extractUsername(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("url rỗng")
	}

	// Nếu chuỗi không có dấu chấm + không có scheme + không có "/"
	// => coi như username thuần (vd: "englunez", "mark.zuckerberg")
	isPureUsername := func(v string) bool {
		if strings.Contains(v, "://") || strings.Contains(v, "/") || strings.Contains(v, " ") {
			return false
		}
		for _, r := range v {
			if !(r == '.' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return false
			}
		}
		return len(v) >= 1 && len(v) <= 100
	}
	if isPureUsername(strings.TrimPrefix(s, "@")) {
		return strings.TrimPrefix(s, "@"), nil
	}

	// Thêm scheme nếu thiếu để url.Parse không lỗi
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("url không hợp lệ: %v", err)
	}

	host := strings.ToLower(u.Host)
	if !strings.Contains(host, "facebook.com") && !strings.Contains(host, "fb.com") && !strings.Contains(host, "fb.me") {
		return "", fmt.Errorf("không phải link Facebook")
	}

	// Trường hợp profile.php?id=... hoặc share.php?...
	if u.Path == "/profile.php" || strings.HasSuffix(u.Path, "/profile.php") {
		id := u.Query().Get("id")
		if id != "" {
			return "id:" + id, nil
		}
	}
	if strings.Contains(u.Path, "share") || u.Path == "/share.php" {
		if id := u.Query().Get("id"); id != "" {
			return "id:" + id, nil
		}
	}

	// Trường hợp /people/Name/1000...  hoặc /user/1000...
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("không tìm thấy username trong url")
	}

	// Bỏ các path noise phổ biến
	noise := map[string]bool{
		"groups": true, "pages": true, "stories": true,
		"watch": true, "events": true, "marketplace": true,
		"photo": true, "photos": true, "video": true, "videos": true,
		"posts": true, "reel": true, "reels": true, "share": true,
		"story.php": true, "photo.php": true, "video.php": true,
		"plugins": true, "login": true, "checkpoint": true,
		"sharer": true, "dialog": true,
		"permalink.php": true, "comment": true, "comments": true,
	}

	first := parts[0]
	if noise[first] {
		// Tìm phần tử tiếp theo
		for _, p := range parts[1:] {
			if !noise[p] && p != "" {
				first = p
				break
			}
		}
	}
	first = strings.TrimSpace(first)
	if first == "" {
		return "", fmt.Errorf("không tìm thấy username trong url")
	}
	// Bỏ query/hash
	if idx := strings.IndexAny(first, "?#"); idx != -1 {
		first = first[:idx]
	}
	// Bỏ @ đầu
	first = strings.TrimPrefix(first, "@")
	return first, nil
}

// Tạo HTTP client với 1 proxy cụ thể
// Nếu proxyURL == nil thì chạy thẳng (no proxy)
//
// Tối ưu cho HIGH CONCURRENCY + fail nhanh khi proxy chết
func makeClient(proxyURL *url.URL, timeoutSec int) *http.Client {
	reqTimeout := time.Duration(timeoutSec) * time.Second
	if proxyURL == nil {
		initFBDirectTransport(timeoutSec)
		if timeoutSec > 12 {
			reqTimeout = 12 * time.Second
		}
		return &http.Client{
			Transport: fbDirectTransport,
			Timeout:   reqTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 8 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
	}
	dialTimeout := 8 * time.Second
	headerTimeout := reqTimeout
	if proxyURL != nil {
		dialTimeout = 8 * time.Second
		// 4G proxy + FB HTML chậm: cap tổng ~22s/lần — tránh 3 lần retry ≈ 33s
		reqTimeout = 22 * time.Second
		headerTimeout = 20 * time.Second
		if timeoutSec < 22 {
			reqTimeout = time.Duration(timeoutSec) * time.Second
			headerTimeout = time.Duration(timeoutSec-2) * time.Second
			if headerTimeout < 12*time.Second {
				headerTimeout = 12 * time.Second
			}
		}
	}
	tr := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          500,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: headerTimeout,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ExpectContinueTimeout: 1 * time.Second,
	}
	tr.Proxy = http.ProxyURL(proxyURL)
	return &http.Client{
		Transport: tr,
		Timeout:   reqTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// ============================================================
// FETCH QUA PROXY (xoay vòng)
// ============================================================
func fetchUIDViaProxy(pool *ProxyPool, cookie, rawTarget, username string, proxyFallbackDirect bool, timeout, retries int) (string, string, string, string, error) {
	// Nếu username có dạng "id:123..." thì trả luôn
	if strings.HasPrefix(username, "id:") {
		id := strings.TrimPrefix(username, "id:")
		return id, "https://www.facebook.com/profile.php?id=" + id, "", "", nil
	}

	fetchOne := func(proxy *url.URL) (string, string, string, string, error) {
		if shouldFetchFullURL(rawTarget, username) {
			return doFetchURL(proxy, cookie, normalizeFBURL(rawTarget), timeout)
		}
		return doFetch(proxy, cookie, username, timeout)
	}

	var lastErr error
	var lastName, lastTitle string
	var lastURL string

	// Qua proxy: chỉ 1 lần (m2proxy session cố định, retry proxy = lãng thời gian)
	if pool != nil {
		proxy := pool.Next()
		uid, finalURL, name, title, err := fetchOne(proxy)
		if err == nil && uid != "" {
			return uid, finalURL, name, title, nil
		}
		lastErr, lastName, lastTitle, lastURL = err, name, title, finalURL
		if proxyFallbackDirect && (isProxyConnectError(err) || isClientTimeout(err)) {
			for attempt := 0; attempt <= retries; attempt++ {
				uid2, u2, n2, t2, err2 := fetchOne(nil)
				if err2 == nil && uid2 != "" {
					return uid2, u2, n2, t2, nil
				}
				lastErr = err2
				lastName, lastTitle, lastURL = n2, t2, u2
				if attempt < retries {
					time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
				}
			}
		}
		return "", lastURL, lastName, lastTitle, lastErr
	}

	for attempt := 0; attempt <= retries; attempt++ {
		uid, finalURL, name, title, err := fetchOne(nil)
		if err == nil && uid != "" {
			return uid, finalURL, name, title, nil
		}
		lastErr = err
		lastName = name
		lastTitle = title
		lastURL = finalURL
		if err != nil && strings.Contains(err.Error(), "uid_not_found") {
			break
		}
		if attempt < retries {
			time.Sleep(time.Duration(150*(attempt+1)) * time.Millisecond)
		}
	}
	return "", lastURL, lastName, lastTitle, lastErr
}

func isClientTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "client.timeout") ||
		strings.Contains(s, "i/o timeout")
}

// isProxyConnectError: không tới được host:port proxy (firewall, sai pass, m2proxy down)
func isProxyConnectError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "proxyconnect") ||
		strings.Contains(s, "proxy connect") ||
		(strings.Contains(s, "dial tcp") && strings.Contains(s, ":8080"))
}

// formatFetchError: message tiếng Việt, không dump cả chuỗi lỗi Go dài
func formatFetchError(err error, useProxy bool) string {
	if err == nil {
		return ""
	}
	if useProxy && isProxyConnectError(err) {
		return "Không kết nối được proxy m2proxy (ipc-4g.m2proxy.com:8080). Kiểm tra mạng/firewall, user:pass trong PROXY_USER, hoặc USE_PROXY=false."
	}
	if useProxy && isClientTimeout(err) {
		return "Proxy 4G/m2proxy hoặc Facebook phản hồi quá chậm (timeout). Đã thử fallback IP trực tiếp nếu PROXY_FALLBACK_DIRECT=true. Có thể giảm RETRIES hoặc tăng chất lượng proxy."
	}
	if strings.Contains(err.Error(), "http_429") || strings.Contains(err.Error(), "http_403") {
		return "Facebook chặn tạm thời (429/403). Thử lại sau hoặc bật proxy USE_PROXY=true."
	}
	if strings.Contains(err.Error(), "wrong_page") {
		return "Facebook redirect sang trang khác (home/feed/me) thay vì profile. Có thể FB_COOKIE đang đăng nhập đè lên — thử xóa FB_COOKIE trong .env hoặc dùng cookie của chính profile đang xem."
	}
	if strings.Contains(err.Error(), "login_or_checkpoint") {
		return "Facebook yêu cầu đăng nhập / checkpoint khi truy cập profile này. Thử đổi FB_COOKIE (cookie tài khoản Facebook đang đăng nhập), bật USE_PROXY=true, hoặc vài phút sau thử lại."
	}
	if strings.Contains(err.Error(), "uid_not_found") {
		return "Không tìm thấy UID trong HTML Facebook (profile ẩn, checkpoint hoặc link không đúng)."
	}
	if strings.Contains(err.Error(), "http_") {
		return "Facebook trả lỗi HTTP: " + err.Error()
	}
	return "Không lấy được UID: " + err.Error()
}

// isFacebookLoginOrCheckpoint: phát hiện Facebook trả về trang đăng nhập / checkpoint
// thay vì trang profile. Khi đó phải thử lại bằng host mobile (mbasic/m) trước khi báo lỗi.
func isFacebookLoginOrCheckpoint(html, finalURL string) bool {
	lowURL := strings.ToLower(finalURL)
	if strings.Contains(lowURL, "facebook.com/login") ||
		strings.Contains(lowURL, "/login/?next=") ||
		strings.Contains(lowURL, "facebook.com/checkpoint") ||
		strings.Contains(lowURL, "/checkpoint/block") ||
		strings.Contains(lowURL, "/checkpoint/") {
		return true
	}
	if len(html) == 0 {
		return false
	}
	low := strings.ToLower(html)
	// Form đăng nhập: name=email + name=pass là dấu hiệu rõ nhất
	if (strings.Contains(low, `name="email"`) || strings.Contains(low, `id="email"`)) &&
		(strings.Contains(low, `name="pass"`) || strings.Contains(low, `id="pass"`)) {
		return true
	}
	if strings.Contains(low, `name="login_form"`) ||
		strings.Contains(low, `id="login_form"`) ||
		strings.Contains(low, `action="/login/`) ||
		strings.Contains(low, `action="/login.php`) ||
		strings.Contains(low, `"checkpoint_required":true`) ||
		strings.Contains(low, `"checkpoint_url"`) ||
		strings.Contains(low, `log in to facebook`) ||
		strings.Contains(low, `log into facebook`) ||
		strings.Contains(low, `please log in`) ||
		(strings.Contains(low, `class="_9o-_`) && strings.Contains(low, `forgot password`)) {
		return true
	}
	return false
}

// swapFacebookHost: đổi host www.facebook.com -> newHost (mbasic/m), giữ nguyên path + query.
// Không phân biệt http/https, www/không www, m/không m.
func swapFacebookHost(rawURL, newHost string) string {
	s := rawURL
	// Nếu chưa có scheme thì ép https
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	for _, old := range []string{
		"https://www.facebook.com",
		"https://m.facebook.com",
		"https://mbasic.facebook.com",
		"https://facebook.com",
		"http://www.facebook.com",
		"http://m.facebook.com",
		"http://mbasic.facebook.com",
		"http://facebook.com",
	} {
		if strings.HasPrefix(s, old) {
			return "https://" + newHost + strings.TrimPrefix(s, old)
		}
	}
	return s
}

func doFetch(proxy *url.URL, cookie, username string, timeout int) (string, string, string, string, error) {
	target := "https://www.facebook.com/" + username
	uid, finalURL, name, title, html, err := doFetchOnce(proxy, cookie, target, "", timeout)
	if uid != "" {
		return uid, finalURL, name, title, nil
	}
	// Nếu Facebook redirect về chrome/home của cookie user (title="Facebook",
	// HTML không chứa username đang query) -> giữ error wrong_page
	if err != nil && strings.Contains(err.Error(), "wrong_page") {
		// Vẫn thử mobile hosts 1 lần trước khi báo lỗi
		for _, host := range []string{"mbasic.facebook.com", "m.facebook.com"} {
			alt := swapFacebookHost(target, host)
			uid2, finalURL2, name2, title2, _, err2 := doFetchOnce(proxy, cookie, alt, "https://"+host+"/", timeout)
			if uid2 != "" {
				return uid2, finalURL2, name2, title2, nil
			}
			if err2 != nil && !strings.Contains(err2.Error(), "wrong_page") && !isRecoverableHTTPError(err2) {
				return "", finalURL, name, title, err2
			}
		}
		return "", finalURL, name, title, fmt.Errorf("wrong_page")
	}
	// Nếu Facebook đá sang trang login/checkpoint -> thử lần lượt mbasic rồi m
	if isFacebookLoginOrCheckpoint(html, finalURL) {
		for _, host := range []string{"mbasic.facebook.com", "m.facebook.com"} {
			alt := swapFacebookHost(target, host)
			uid2, finalURL2, name2, title2, _, err2 := doFetchOnce(proxy, cookie, alt, "https://"+host+"/", timeout)
			if uid2 != "" {
				return uid2, finalURL2, name2, title2, nil
			}
			// Nếu mobile trả lỗi không phải login thì trả lỗi gốc luôn
			if err2 != nil && !isRecoverableHTTPError(err2) {
				return "", finalURL, name, title, err2
			}
		}
		// Cả www + mbasic + m đều dính login -> trả lỗi riêng
		return "", finalURL, name, title, fmt.Errorf("login_or_checkpoint")
	}
	if err != nil && !isRecoverableHTTPError(err) {
		return "", finalURL, name, title, err
	}

	// Fallback search (chậm) — tối đa 2 lần; không dùng khi proxy
	if name != "" && proxy == nil {
		candidates := nameCandidates(name)
		if len(candidates) > 2 {
			candidates = candidates[:2]
		}
		searchTO := timeout
		if searchTO > 10 {
			searchTO = 10
		}
		for _, n := range candidates {
			uid2, errURL, errName, err2 := searchUIDByName(nil, cookie, n, searchTO)
			if err2 == nil && uid2 != "" {
				return uid2, errURL, n, errName, nil
			}
		}
	}
	return "", finalURL, name, title, fmt.Errorf("uid_not_found")
}

// isRecoverableHTTPError: lỗi 4xx/5xx / rate-limit / wrong_page có thể thử host khác
func isRecoverableHTTPError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "http_4") || strings.Contains(s, "http_5") ||
		strings.Contains(s, "rate_limit") || strings.Contains(s, "wrong_page")
}

// doFetchOnce: 1 lần HTTP GET + parse UID. Trả về html để caller quyết định retry.
func doFetchOnce(proxy *url.URL, cookie, target, referer string, timeout int) (uid, finalURL, name, title, html string, err error) {
	client := makeClient(proxy, timeout)
	req, errReq := http.NewRequest("GET", target, nil)
	if errReq != nil {
		return "", "", "", "", "", errReq
	}
	setHeaders(req, cookie, referer)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", "", "", "", errDo
	}
	defer resp.Body.Close()

	finalURL = target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode == 429 || resp.StatusCode == 403 {
		return "", finalURL, "", "", "", fmt.Errorf("http_%d_rate_limit", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", finalURL, "", "", "", fmt.Errorf("http_%d", resp.StatusCode)
	}

	// Phát hiện redirect về home/feed/me — tránh nhầm UID tài khoản cookie
	if isWrongPage(target, finalURL, "") {
		// Đọc hết body để trả về cho debug, nhưng báo error để caller xử lý
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		return "", finalURL, "", "", string(bodyBytes), fmt.Errorf("wrong_page")
	}

	html, earlyUID, earlyName, earlyTitle, errRead := readFacebookHTMLUntilUID(resp.Body, finalURL)
	if errRead != nil {
		return "", finalURL, "", "", "", errRead
	}
	// Check lại sau khi có HTML: cookie user có thể được serve chrome/home page
	// dù URL không redirect (title="Facebook", không chứa username đang query)
	if isWrongPage(target, finalURL, html) {
		return "", finalURL, "", "", html, fmt.Errorf("wrong_page")
	}
	if earlyUID != "" {
		name, title = earlyName, earlyTitle
		if name == "" {
			name = title
		}
		if title == "" {
			title = extractTitle(html)
		}
		if name == "" {
			name = extractName(html)
		}
		if name == "" {
			name = title
		}
		return earlyUID, finalURL, name, title, html, nil
	}
	title = extractTitle(html)
	name = extractName(html)
	return "", finalURL, name, title, html, nil
}

func resolveResponseUsername(inputUser, finalURL, uid string) string {
	if v := vanityFromFinalURL(finalURL); v != "" {
		return v
	}
	if resolved := usernameFromResolvedURL(finalURL); resolved != "" {
		r := displayUsername(resolved)
		if r != "share" && !looksLikeShareSlug(r) && !isFBPathNoise(r) {
			return r
		}
	}
	if inputUser != "" && inputUser != "share" && !looksLikeShareSlug(inputUser) && !isFBPathNoise(inputUser) {
		return displayUsername(inputUser)
	}
	if uid != "" {
		return uid
	}
	return inputUser
}

func isFBPathNoise(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	noise := map[string]bool{
		"share": true, "sharer": true, "plugins": true, "login": true, "checkpoint": true,
		"groups": true, "pages": true, "watch": true, "dialog": true,
	}
	return noise[s]
}

func vanityFromFinalURL(finalURL string) string {
	u, err := url.Parse(finalURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 1 && parts[0] != "" &&
		!looksLikeShareSlug(parts[0]) &&
		!(isAllDigits(parts[0]) && len(parts[0]) >= 8) &&
		parts[0] != "profile.php" && parts[0] != "share" {
		return parts[0]
	}
	return ""
}

func displayUsername(s string) string {
	if strings.HasPrefix(s, "id:") {
		return strings.TrimPrefix(s, "id:")
	}
	return s
}

func usernameFromResolvedURL(finalURL string) string {
	u, err := url.Parse(finalURL)
	if err != nil {
		return ""
	}
	path := u.Path
	if path == "/profile.php" || strings.HasSuffix(path, "/profile.php") {
		if id := u.Query().Get("id"); id != "" {
			return "id:" + id
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	noise := map[string]bool{
		"groups": true, "pages": true, "stories": true, "watch": true, "events": true,
		"marketplace": true, "photo": true, "photos": true, "video": true, "videos": true,
		"posts": true, "reel": true, "reels": true, "share": true, "plugins": true,
		"login": true, "checkpoint": true, "sharer": true, "dialog": true,
	}
	if parts[0] == "people" && len(parts) >= 3 && isAllDigits(parts[len(parts)-1]) {
		return "id:" + parts[len(parts)-1]
	}
	first := parts[0]
	if noise[first] {
		for _, p := range parts[1:] {
			if !noise[p] && p != "" && !looksLikeShareSlug(p) {
				first = p
				break
			}
		}
	}
	first = strings.TrimPrefix(strings.TrimSpace(first), "@")
	if first == "" || looksLikeShareSlug(first) {
		return ""
	}
	if isAllDigits(first) && len(first) >= 8 {
		return "id:" + first
	}
	return first
}

// canonicalProfileURL: URL sạch — profile số → profile.php?id=; vanity → /username
func canonicalProfileURL(finalURL, uid string) string {
	baseHost := "www.facebook.com"
	uid = strings.TrimSpace(uid)
	if uid != "" && isAllDigits(uid) && len(uid) >= 8 {
		return "https://" + baseHost + "/profile.php?id=" + uid
	}
	u, err := url.Parse(finalURL)
	if err != nil {
		return strings.TrimSpace(finalURL)
	}
	host := strings.ToLower(u.Host)
	if !strings.Contains(host, "facebook.com") && !strings.Contains(host, "fb.com") {
		return finalURL
	}
	if u.Path == "/profile.php" || strings.HasSuffix(u.Path, "/profile.php") {
		if id := u.Query().Get("id"); id != "" {
			return "https://" + baseHost + "/profile.php?id=" + id
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "people" && isAllDigits(parts[len(parts)-1]) {
		return "https://" + baseHost + "/profile.php?id=" + parts[len(parts)-1]
	}
	if len(parts) == 1 && parts[0] != "" && !looksLikeShareSlug(parts[0]) && !(isAllDigits(parts[0]) && len(parts[0]) >= 8) {
		return "https://" + baseHost + "/" + parts[0]
	}
	un := usernameFromResolvedURL(finalURL)
	if un != "" && !strings.HasPrefix(un, "id:") {
		return "https://" + baseHost + "/" + un
	}
	if strings.HasPrefix(un, "id:") {
		return "https://" + baseHost + "/profile.php?id=" + strings.TrimPrefix(un, "id:")
	}
	return "https://" + baseHost + "/"
}

func normalizeFBURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	return s
}

// Link share / slug lạ (vd 1Tkq96VukR) — cần GET đúng URL gốc để FB redirect, không chỉ /slug
func shouldFetchFullURL(rawTarget, username string) bool {
	raw := strings.ToLower(rawTarget)
	if strings.Contains(raw, "share.php") ||
		strings.Contains(raw, "/share/") ||
		strings.Contains(raw, "permalink.php") ||
		strings.Contains(raw, "story.php") ||
		strings.Contains(raw, "photo.php") ||
		strings.Contains(raw, "video.php") ||
		strings.Contains(raw, "watch?") ||
		strings.Contains(raw, "story_fbid=") ||
		strings.Contains(raw, "fbid=") ||
		strings.Contains(raw, "set=a.") {
		return true
	}
	if u, err := url.Parse(normalizeFBURL(rawTarget)); err == nil {
		if id := u.Query().Get("id"); id != "" && len(id) >= 8 {
			return true
		}
	}
	return looksLikeShareSlug(username)
}

func looksLikeShareSlug(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 6 || len(s) > 80 {
		return false
	}
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "pfbid") {
		return true
	}
	hasUpper, hasDigit, hasDot := false, false, false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.':
			hasDot = true
		}
	}
	if hasUpper && hasDigit && !hasDot {
		return true
	}
	if len(s) >= 12 && !hasDot && hasDigit {
		return true
	}
	return false
}

func doFetchURL(proxy *url.URL, cookie, targetURL string, timeout int) (string, string, string, string, error) {
	uid, finalURL, name, title, html, err := doFetchOnce(proxy, cookie, targetURL, "", timeout)
	if uid != "" {
		return uid, finalURL, name, title, nil
	}
	if isFacebookLoginOrCheckpoint(html, finalURL) {
		for _, host := range []string{"mbasic.facebook.com", "m.facebook.com"} {
			alt := swapFacebookHost(targetURL, host)
			uid2, finalURL2, name2, title2, _, err2 := doFetchOnce(proxy, cookie, alt, "https://"+host+"/", timeout)
			if uid2 != "" {
				return uid2, finalURL2, name2, title2, nil
			}
			if err2 != nil && !isRecoverableHTTPError(err2) {
				return "", finalURL, name, title, err2
			}
		}
		return "", finalURL, name, title, fmt.Errorf("login_or_checkpoint")
	}
	if err != nil && !isRecoverableHTTPError(err) {
		return "", finalURL, name, title, err
	}

	// Thử lấy UID từ finalURL / share page
	if uid2 := uidFromFinalURL(finalURL); uid2 != "" {
		return uid2, finalURL, name, title, nil
	}
	if uid2 := extractSharePageUID(html); uid2 != "" {
		return uid2, finalURL, name, title, nil
	}

	if name != "" && proxy == nil {
		candidates := nameCandidates(name)
		if len(candidates) > 2 {
			candidates = candidates[:2]
		}
		searchTO := timeout
		if searchTO > 10 {
			searchTO = 10
		}
		for _, n := range candidates {
			uid2, errURL, errName, err2 := searchUIDByName(nil, cookie, n, searchTO)
			if err2 == nil && uid2 != "" {
				return uid2, errURL, n, errName, nil
			}
		}
	}
	return "", finalURL, name, title, fmt.Errorf("uid_not_found")
}

var (
	reProfileIDQuery = regexp.MustCompile(`[?&]id=(\d{8,})`)
	rePeoplePathID   = regexp.MustCompile(`facebook\.com/people/[^/]+/(\d{8,})`)
	rePathNumericID  = regexp.MustCompile(`facebook\.com/(?:groups/[^/]+/(?:user|members)/|profile\.php\?id=)(\d{8,})`)
)

func uidFromFinalURL(finalURL string) string {
	if m := reProfileIDQuery.FindStringSubmatch(finalURL); len(m) >= 2 {
		return m[1]
	}
	if m := rePeoplePathID.FindStringSubmatch(finalURL); len(m) >= 2 {
		return m[1]
	}
	if m := rePathNumericID.FindStringSubmatch(finalURL); len(m) >= 2 {
		return m[1]
	}
	if u, err := url.Parse(finalURL); err == nil {
		if id := u.Query().Get("id"); len(id) >= 8 {
			return id
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if len(parts[i]) >= 8 && isAllDigits(parts[i]) {
				return parts[i]
			}
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// extractExpectedUIDFromURL: lấy UID mà URL hứa hẹn (?id=X hoặc /people/Name/X).
// Dùng để cross-validate UID tìm được trong HTML — tránh match nhầm UID của tài khoản
// đang đăng nhập (cookie) khi Facebook trả về trang có cả data của cookie user lẫn profile user.
func extractExpectedUIDFromURL(u string) string {
	if u == "" {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	// ?id=X hoặc ?id=X&... hoặc các query khác có id=
	if id := parsed.Query().Get("id"); id != "" && len(id) >= 8 && isAllDigits(id) {
		return id
	}
	// /people/Name/UID
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "people" {
		last := parts[len(parts)-1]
		if isAllDigits(last) && len(last) >= 8 {
			return last
		}
	}
	return ""
}

// isWrongPage: phát hiện Facebook trả về sai trang (chrome/home của cookie user
// thay vì profile đang query). Khi đó, mọi "userID" trong HTML là của tài khoản
// cookie, KHÔNG phải của profile đang xem — phải trả về error thay vì nhầm UID.
//
// Có 2 dạng:
//
//	(A) HTTP redirect sang /home.php, /feed/, /me/, ...  -> bắt được bằng finalURL
//	(B) Vẫn ở URL /username nhưng HTML là chrome/home (title="Facebook",
//	    og:title="Facebook", HTML không chứa username/id của profile) ->
//	    bắt được bằng pageMentionsTarget
func isWrongPage(target, finalURL, html string) bool {
	if finalURL != "" {
		low := strings.ToLower(finalURL)
		badSubstrings := []string{
			"/home.php", "/?sk=h_n", "/?sk=h_chr", "/?ref=", "/me/", "/feed/",
			"/watch/", "/notifications", "/marketplace", "/friends",
			"/settings", "/login.php", "/login/?next=", "/checkpoint/",
		}
		for _, b := range badSubstrings {
			if strings.Contains(low, b) {
				return true
			}
		}
		tp, err1 := url.Parse(target)
		fp, err2 := url.Parse(finalURL)
		if err1 == nil && err2 == nil {
			tpPath := strings.Trim(tp.Path, "/")
			fpPath := strings.Trim(fp.Path, "/")
			if tpPath != "" && tpPath != "profile.php" && !looksLikeShareSlug(tpPath) {
				if fpPath == "" {
					return true
				}
				if fpPath != "profile.php" && !strings.HasPrefix(fpPath, "people/") &&
					!strings.HasPrefix(fpPath, "share/") && fpPath != tpPath {
					return true
				}
			}
		}
	}
	// (B) HTML không chứa username/id của profile đang query -> chrome/home của cookie user
	if html != "" && !pageMentionsTarget(target, html) {
		return true
	}
	return false
}

// pageMentionsTarget: kiểm tra HTML có chứa username hoặc UID của profile đang query không.
// - Với ?id=X: tìm chuỗi X trong HTML
// - Với /people/Name/UID: tìm UID
// - Với /username: tìm username (case-insensitive)
func pageMentionsTarget(target, html string) bool {
	targetURL := normalizeFBURL(target)
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed == nil {
		return true
	}
	htmlLow := strings.ToLower(html)
	// ?id=X
	if id := parsed.Query().Get("id"); id != "" && len(id) >= 6 {
		if strings.Contains(html, id) {
			return true
		}
	}
	// /people/Name/UID
	path := strings.Trim(parsed.Path, "/")
	if strings.HasPrefix(path, "people/") {
		parts := strings.Split(path, "/")
		if len(parts) >= 3 && len(parts[2]) >= 6 {
			if strings.Contains(html, parts[2]) {
				return true
			}
		}
	}
	// /username
	if path != "" && path != "profile.php" && !strings.HasPrefix(path, "people/") {
		if strings.Contains(htmlLow, strings.ToLower(path)) {
			return true
		}
	}
	return false
}

// extractOGTitle: lấy og:title (tên hiển thị của profile, vd "Nguyễn Duy Khánh")
func extractOGTitle(html string) string {
	if m := reOGTitle.FindStringSubmatch(html); len(m) >= 2 {
		return htmlUnescape(m[1])
	}
	return ""
}

// extractOGURL: lấy og:url (canonical URL của profile đang xem)
func extractOGURLAny(html string) string {
	if m := reOGURLAny.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// htmlUnescape: giải mã các entity HTML phổ biến (&amp; &quot; &#xNN;)
func htmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#039;", "'")
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
}

var (
	reShareOwnerID   = regexp.MustCompile(`"owning_profile"\s*:\s*\{[^}]*"id"\s*:\s*"(\d+)"`)
	reShareActorID   = regexp.MustCompile(`"actorID"\s*:\s*"(\d+)"`)
	reSharePageID    = regexp.MustCompile(`"pageID"\s*:\s*"(\d+)"`)
	reShareContentID = regexp.MustCompile(`"content_owner_id_new"\s*:\s*"(\d+)"`)
	reMetaAlIOS      = regexp.MustCompile(`property="al:ios:url"\s+content="fb://profile/(\d+)"`)
)

func extractSharePageUID(html string) string {
	for _, re := range []*regexp.Regexp{reMetaAlIOS, reShareOwnerID, reShareActorID, reShareContentID, reSharePageID} {
		if m := re.FindStringSubmatch(html); len(m) >= 2 && len(m[1]) >= 8 {
			return m[1]
		}
	}
	if uid := extractFromAppURL(html); uid != "" {
		return uid
	}
	if m := reOGURL.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	// og:url dạng /username hoặc story — thử bản rộng hơn
	reOGWide := regexp.MustCompile(`property="og:url"\s+content="https?://[^"]*facebook\.com[^"]*(\d{8,})`)
	if m := reOGWide.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// tryParseUID — thứ tự ưu tiên MỚI (đã fix bug nhầm UID cookie):
//
//  1. og:url — luôn là canonical URL của profile đang xem (kể cả khi đã đăng nhập).
//  2. fb://profile/X trong og:url hoặc al:ios:url — cũng là canonical.
//  3. shouldUseFXIMProfilePicEditor:false,"userID":"..." — chỉ chấp nhận khi trùng
//     expectedUID (URL ?id= hoặc /people/Name/UID); nếu không khớp thì bỏ qua vì
//     có thể là của tài khoản đang đăng nhập (cookie user), không phải profile.
//  4. Generic "userID":"..." — cũng chỉ chấp nhận khi trùng expectedUID.
//  5. extractFromJSON — regex rộng, chỉ dùng cuối cùng khi đã hết cách.
//
// finalURL dùng để cross-validate với expectedUID.
func tryParseUID(html, finalURL string) (uid, name, title string) {
	expected := extractExpectedUIDFromURL(finalURL)

	// Thu thập tất cả candidate theo priority
	type cand struct {
		u   string
		src string
	}
	var candidates []cand
	if u := extractFromOGURL(html); u != "" {
		candidates = append(candidates, cand{u, "og_url"})
	}
	if u := extractFromAppURL(html); u != "" {
		candidates = append(candidates, cand{u, "app_url"})
	}
	if u := extractFromLegacyUserID(html); u != "" {
		candidates = append(candidates, cand{u, "legacy_user_id"})
	}
	if u := extractUserID(html); u != "" {
		candidates = append(candidates, cand{u, "user_id"})
	}
	if u := extractFromJSON(html); u != "" {
		candidates = append(candidates, cand{u, "json"})
	}

	name = fastExtractName(html)
	title = fastExtractTitle(html)

	// Nếu URL có expectedUID -> chỉ chấp nhận candidate trùng expectedUID
	if expected != "" {
		for _, c := range candidates {
			if c.u == expected {
				return c.u, name, title
			}
		}
		// Không match: trả "" để caller xử lý (fallback search, mobile retry, ...)
		return "", name, title
	}

	// Không có expectedUID: dùng candidate đầu tiên theo priority (đã sort)
	for _, c := range candidates {
		return c.u, name, title
	}
	return "", name, title
}

func fastExtractTitle(html string) string {
	if i := strings.Index(html, "<title"); i >= 0 {
		s := html[i:]
		if len(s) > 12000 {
			s = s[:12000]
		}
		return extractTitle(s)
	}
	return ""
}

func fastExtractName(html string) string {
	if len(html) > 200000 {
		html = html[:200000]
	}
	return extractName(html)
}

// readFacebookHTMLUntilUID: chunk 32KB, dừng ngay khi có UID hợp lệ (qua cross-validation).
// finalURL dùng để cross-validate, tránh match nhầm UID của cookie user.
func readFacebookHTMLUntilUID(r io.Reader, finalURL string) (html, uid, name, title string, err error) {
	const chunk = 32 * 1024
	const maxTotal = 384 * 1024
	expected := extractExpectedUIDFromURL(finalURL)
	var buf []byte
	tmp := make([]byte, chunk)
	for len(buf) < maxTotal {
		n, readErr := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			uid, name, title = tryParseUID(string(buf), finalURL)
			if uid != "" {
				return string(buf), uid, name, title, nil
			}
			// Nếu đã biết expectedUID và đã thấy candidate khác expectedUID
			// thì không cần đọc tiếp chunk làm gì (vẫn phải đọc tới maxTotal
			// để các extractor mới như og:url có cơ hội xuất hiện).
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if len(buf) > 0 {
				return string(buf), "", "", "", nil
			}
			return "", "", "", "", readErr
		}
	}
	s := string(buf)
	uid, name, title = tryParseUID(s, finalURL)
	_ = expected
	return s, uid, name, title, nil
}

// ============================================================
// TIỆN ÍCH
// ============================================================
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024*4)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexAny(line, " \t"); idx != -1 {
			line = line[:idx]
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

var reAppURL = regexp.MustCompile(`fb://profile/(\d+)`)

func extractFromAppURL(html string) string {
	m := reAppURL.FindStringSubmatch(html)
	if len(m) >= 2 {
		if _, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return m[1]
		}
	}
	return ""
}

// Regex gốc PHP: "shouldUseFXIMProfilePicEditor":false,"userID":"123"
var reLegacyUserID = regexp.MustCompile(`"shouldUseFXIMProfilePicEditor"\s*:\s*false\s*,\s*"userID"\s*:\s*"(\d+)"`)

func extractFromLegacyUserID(html string) string {
	m := reLegacyUserID.FindStringSubmatch(html)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

var reOGURLAny = regexp.MustCompile(`property="og:url"\s+content="([^"]+)"`)

// og:url dạng https://www.facebook.com/people/Name/1000... hoặc /profile/1000...
var reOGURL = regexp.MustCompile(`property="og:url"\s+content="https?://(?:www\.|m\.)?facebook\.com/(?:people/[^/]+/|profile\.php\?id=)?(\d+)"`)

func extractFromOGURL(html string) string {
	m := reOGURL.FindStringSubmatch(html)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

var reUserID = regexp.MustCompile(`"userID"\s*:\s*"(\d+)"`)

func extractUserID(html string) string {
	m := reUserID.FindStringSubmatch(html)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

var reJSON = regexp.MustCompile(`"(?:profile_id|entity_id|user_id|id)"\s*:\s*"(\d+)"`)

func extractFromJSON(html string) string {
	m := reJSON.FindStringSubmatch(html)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

var reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func extractTitle(html string) string {
	m := reTitle.FindStringSubmatch(html)
	if len(m) >= 2 {
		t := strings.TrimSpace(m[1])
		t = strings.TrimPrefix(t, "Facebook - ")
		return t
	}
	return ""
}

// Trích name từ các thẻ meta trong HTML Facebook
// Ưu tiên: og:title -> twitter:title -> og:image:alt -> <title>
// Decode HTML entities luôn (vd: &#x111; -> đ, &#039; -> ')
var (
	reOGTitle    = regexp.MustCompile(`(?is)<meta\s+property="og:title"\s+content="([^"]+)"`)
	reTwitterTtl = regexp.MustCompile(`(?is)<meta\s+name="twitter:title"\s+content="([^"]+)"`)
	reOGImageAlt = regexp.MustCompile(`(?is)<meta\s+property="og:image:alt"\s+content="([^"]+)"`)
	reAppNameTtl = regexp.MustCompile(`(?is)<meta\s+property="al:ios:app_name"\s+content="([^"]+)"`)
)

func extractName(html string) string {
	get := func(re *regexp.Regexp) string {
		m := re.FindStringSubmatch(html)
		if len(m) >= 2 {
			return decodeHTMLEntities(strings.TrimSpace(m[1]))
		}
		return ""
	}
	// 1. og:title (thường có dạng "Minh Anh" hoặc "Minh Anh | Facebook")
	if v := get(reOGTitle); v != "" {
		return cleanName(v)
	}
	// 2. twitter:title
	if v := get(reTwitterTtl); v != "" {
		return cleanName(v)
	}
	// 3. og:image:alt
	if v := get(reOGImageAlt); v != "" {
		return cleanName(v)
	}
	// 4. <title> fallback
	if t := extractTitle(html); t != "" {
		return cleanName(t)
	}
	return ""
}

// Làm sạch name: bỏ suffix " | Facebook", " - Facebook", "(@xxx) • Facebook"...
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	// Bỏ các suffix phổ biến
	suffixes := []string{
		" | Facebook", " - Facebook", ", Facebook",
		" • Facebook", " · Facebook", " on Facebook",
		" (@Facebook)", " - Trang cá nhân", " - Home",
		" | Trang cá nhân", " - trang cá nhân",
	}
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			s = strings.TrimSuffix(s, suf)
		}
	}
	// Bỏ phần " (@username) •" hoặc "(@username)" ở cuối
	if i := strings.Index(s, " (@"); i > 0 {
		s = s[:i]
	}
	// Bỏ phần " - Xem trang cá nhân..." nếu có
	if i := strings.Index(s, " - "); i > 0 {
		// Cẩn thận: "Nguyễn Văn A - Trang cá nhân" → bỏ phần sau
		after := strings.ToLower(s[i+3:])
		if strings.Contains(after, "trang") || strings.Contains(after, "facebook") || strings.Contains(after, "home") {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// Decode HTML entities phổ biến trong FB
func decodeHTMLEntities(s string) string {
	// Numeric &#xNN; và &#NN;
	s = numericEntityRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[2 : len(match)-1]
		var n int64
		if strings.HasPrefix(inner, "x") || strings.HasPrefix(inner, "X") {
			n, _ = strconv.ParseInt(inner[1:], 16, 32)
		} else {
			n, _ = strconv.ParseInt(inner, 10, 32)
		}
		if n == 0 {
			return ""
		}
		return string(rune(n))
	})
	// Named entities phổ biến
	repl := map[string]string{
		"&amp;":    "&",
		"&lt;":     "<",
		"&gt;":     ">",
		"&quot;":   `"`,
		"&apos;":   "'",
		"&nbsp;":   " ",
		"&copy;":   "©",
		"&reg;":    "®",
		"&trade;":  "™",
		"&hellip;": "…",
		"&mdash;":  "—",
		"&ndash;":  "–",
		"&lsquo;":  "‘",
		"&rsquo;":  "’",
		"&ldquo;":  "“",
		"&rdquo;":  "”",
		"&middot;": "·",
		"&bull;":   "•",
		"&deg;":    "°",
		"&euro;":   "€",
		"&pound;":  "£",
		"&cent;":   "¢",
		"&sect;":   "§",
		"&para;":   "¶",
		"&times;":  "×",
		"&divide;": "÷",
		"&plusmn;": "±",
		"&frac12;": "½",
		"&frac14;": "¼",
		"&frac34;": "¾",
	}
	for k, v := range repl {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

var numericEntityRe = regexp.MustCompile(`&#(x?[0-9a-fA-F]+);`)

// Tách thành các biến thể tên để search
// VD: "Minh Anh" -> ["Minh Anh", "MinhAnh", "minh anh", "minhanh"]
func nameCandidates(title string) []string {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(title)
	add(strings.ReplaceAll(title, " ", ""))
	add(strings.ToLower(title))
	add(strings.ToLower(strings.ReplaceAll(title, " ", "")))
	// Từng phần (firstname, lastname)
	parts := strings.Fields(title)
	if len(parts) >= 2 {
		add(parts[0])
		add(parts[len(parts)-1])
		add(parts[0] + " " + parts[len(parts)-1])
	}
	return out
}

// Gọi Facebook Search public endpoint, parse UID của kết quả đầu tiên
func searchUIDByName(proxy *url.URL, cookie, name string, timeout int) (string, string, string, error) {
	client := makeClient(proxy, timeout)
	q := url.QueryEscape(name)
	target := "https://www.facebook.com/search/people/?q=" + q

	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return "", "", "", err
	}
	// Search thì đổi accept & sec-fetch sang JSON-ish, nhưng HTML vẫn ok
	setHeaders(req, cookie, "https://www.facebook.com/")
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-site", "same-origin")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode == 429 || resp.StatusCode == 403 {
		return "", finalURL, name, fmt.Errorf("http_%d_rate_limit", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", finalURL, name, fmt.Errorf("http_%d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 3*1024*1024))
	if err != nil {
		return "", finalURL, name, err
	}
	html := string(body)

	// Lấy name từ trang search
	searchName := extractName(html)
	if searchName == "" {
		searchName = name
	}

	// Ưu tiên 1: link profile trong HTML search result
	// FB search trả về các link /<username> hoặc /profile.php?id=ID hoặc /people/Name/ID
	if uid := extractFirstProfileIDFromSearch(html); uid != "" {
		return uid, finalURL, searchName, nil
	}
	// Ưu tiên 2: thử lấy từ fb://profile/ID
	if uid := extractFromAppURL(html); uid != "" {
		return uid, finalURL, searchName, nil
	}
	// Ưu tiên 3: og:url
	if uid := extractFromOGURL(html); uid != "" {
		return uid, finalURL, searchName, nil
	}
	// Ưu tiên 4: JSON userID
	if uid := extractUserID(html); uid != "" {
		return uid, finalURL, searchName, nil
	}
	// Ưu tiên 5: Graph API typeahead endpoint (cũ, có thể đã chết)
	if uid := tryGraphQLTypeahead(proxy, cookie, name, timeout); uid != "" {
		return uid, finalURL, searchName, nil
	}
	return "", finalURL, searchName, fmt.Errorf("uid_not_found_in_search")
}

// Tìm UID đầu tiên trong HTML search result
// Các pattern hay gặp:
//
//	href="/people/Name/123456"
//	href="/profile.php?id=123456"
//	"/user/123456"
//	"profile_id":"123456"
//	"/100012345678901"
var reSearchProfile1 = regexp.MustCompile(`href="https?://(?:www\.|m\.)?facebook\.com/people/[^/]+/(\d+)"`)
var reSearchProfile2 = regexp.MustCompile(`href="https?://(?:www\.|m\.)?facebook\.com/profile\.php\?id=(\d+)"`)
var reSearchProfile3 = regexp.MustCompile(`"/(\d{10,})(?:"|/|\?)`) // user_id 10+ số

func extractFirstProfileIDFromSearch(html string) string {
	if m := reSearchProfile1.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	if m := reSearchProfile2.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	if m := reSearchProfile3.FindStringSubmatch(html); len(m) >= 2 {
		return m[1]
	}
	if uid := extractUserID(html); uid != "" {
		return uid
	}
	return ""
}

// Gọi GraphQL typeahead endpoint (cũ vẫn còn hoạt động 1 số trường hợp)
func tryGraphQLTypeahead(proxy *url.URL, cookie, name string, timeout int) string {
	client := makeClient(proxy, timeout)
	target := "https://www.facebook.com/ajax/typeahead/search.php?__a=1&filter[]=user&query=" + url.QueryEscape(name)
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return ""
	}
	setHeaders(req, cookie, "https://www.facebook.com/")
	req.Header.Set("accept", "*/*")
	req.Header.Set("x-requested-with", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return ""
	}
	html := string(body)
	if uid := extractFromAppURL(html); uid != "" {
		return uid
	}
	if uid := extractUserID(html); uid != "" {
		return uid
	}
	return ""
}
