# GetID Facebook — tool_facebook (DichVuRight)

API + trang web lấy **UID Facebook** từ link profile, share, `profile.php?id=`, v.v. Viết bằng **Go**, nhúng sẵn giao diện HTML.

---

## Yêu cầu

| Thành phần | Ghi chú |
|------------|---------|
| **Go** | Phiên bản **≥ 1.21** (project dùng `go 1.26.3` trong `go.mod`) — [https://go.dev/dl/](https://go.dev/dl/) |
| **Git** | Tùy chọn (clone repo) |
| **.env** | Đặt cạnh binary hoặc thư mục chạy lệnh |

Không cần Node/npm — frontend nằm trong `static/` và được `go:embed`.

---

## Cấu trúc thư mục

```
tool_facebook/
├── main.go          # API, scrape FB, proxy
├── static.go        # Embed static/*
├── static/
│   └── index.html   # Trang GetID
├── go.mod
├── .env             # Cấu hình (tự tạo, không commit cookie thật)
├── README.md
└── tool_facebook.exe / tool_facebook   # Sau khi build
```

---

## Setup nhanh

### 1. Clone / copy project

```bash
cd d:\tool_facebook   # hoặc đường dẫn project của bạn
```

### 2. Tạo file `.env`

Copy mẫu và chỉnh (đặt **cùng thư mục** với file exe/binary khi chạy):

```env
# Cookie đăng nhập FB (lấy từ trình duyệt khi đã login facebook.com)
FB_COOKIE=datr=...; sb=...; ...

# Cổng API + web (KHÁC cổng proxy :8080)
LISTEN=:8787

# Proxy (chỉ dùng khi USE_PROXY=true)
PROXY_HOST=
PROXY_USER=
PROXY_POOL=

# false = gọi Facebook bằng IP máy chủ | true = qua proxy
USE_PROXY=false

# Khi USE_PROXY=true: proxy lỗi thì thử lại không proxy
PROXY_FALLBACK_DIRECT=true

TIMEOUT=12
RETRIES=0
```

**Lưu ý:** `FB_COOKIE` hết hạn theo thời gian — cần cập nhật khi Facebook trả lỗi / không parse được UID.

### 3. Build

Xem mục [Build](#build) bên dưới.

### 4. Chạy

**Windows:**

```powershell
.\tool_facebook.exe
```

**Linux:**

```bash
chmod +x ./tool_facebook
./tool_facebook
```

Console nên có:

- `>> Đã load .env: ...`
- `>> Proxy RA NGOÀI: TẮT` hoặc `BẬT`
- `>> Server đang lắng nghe tại :8787`

### 5. Truy cập

| Mục | URL |
|-----|-----|
| Trang web | http://127.0.0.1:8787/ |
| API | http://127.0.0.1:8787/api/v1/facebook |
| Health | http://127.0.0.1:8787/health |

**Không** mở `localhost:8080` để dùng tool — đó thường là cổng proxy outbound, không phải `LISTEN`.

---

## Build

Chạy trong thư mục gốc project (có `go.mod`).

### Windows (exe trên máy Windows)

```powershell
go build -o tool_facebook.exe .
```

Chạy:

```powershell
.\tool_facebook.exe
```

### Linux (binary trên máy Linux)

```bash
go build -o tool_facebook .
chmod +x tool_facebook
./tool_facebook
```

### Cross-compile: build **trên Windows** ra binary **Linux**

**Linux amd64** (VPS Ubuntu/Debian 64-bit):

```powershell
$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"
go build -o tool_facebook .
```

**Linux arm64** (một số VPS ARM / Raspberry Pi 64-bit):

```powershell
$env:GOOS="linux"
$env:GOARCH="arm64"
$env:CGO_ENABLED="0"
go build -o tool_facebook .
```

Copy file `tool_facebook` + `.env` lên server Linux, `chmod +x`, chạy.

### Cross-compile: build **trên Linux** ra exe **Windows**

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o tool_facebook.exe .
```

### Build tối giản (một dòng)

| Đích | Lệnh (PowerShell) | Lệnh (Bash) |
|------|-------------------|-------------|
| Windows exe | `go build -o tool_facebook.exe .` | `GOOS=windows GOARCH=amd64 go build -o tool_facebook.exe .` |
| Linux amd64 | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tool_facebook .` | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tool_facebook .` |
| Linux arm64 | `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o tool_facebook .` | `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o tool_facebook .` |

`CGO_ENABLED=0` giúp binary tĩnh, dễ copy sang máy khác không cần cài thư viện C.

---

## API

### `GET` / `POST` `/api/v1/facebook`

| Tham số | Mô tả |
|---------|--------|
| `url` | Link Facebook đầy đủ (profile, share, `profile.php?id=`, …) |
| `username` | Username hoặc slug (nếu không truyền `url`) |

**Ví dụ:**

```bash
curl "http://127.0.0.1:8787/api/v1/facebook?url=https://www.facebook.com/dichvurightvn"
curl "http://127.0.0.1:8787/api/v1/facebook?username=dichvurightvn"
curl "http://127.0.0.1:8787/api/v1/facebook?url=https://www.facebook.com/share/1FnzFuJRpe/"
```

**Response thành công (rút gọn):**

```json
{
  "status": "success",
  "id": "100002839470854",
  "username": "dichvurightvn",
  "name": "Nguyễn Duy Khánh",
  "url": "https://www.facebook.com/dichvurightvn",
  "author": "DichVuRight",
  "time_check": "2026-06-11T23:05:29+07:00",
  "time_elapsed": 0.703489
}
```

Link **share** sau redirect có thể trả `username` vanity (`dichvurightvn`) hoặc UID; `url` được làm sạch (bỏ `mibextid`, `share_url`, …).

### `GET` `/health`

```json
{
  "status": "ok",
  "proxy_outbound": false,
  "proxy_host": "...",
  "fallback_direct": true
}
```

---

## Biến môi trường (`.env`)

| Biến | Mặc định | Ý nghĩa |
|------|----------|---------|
| `FB_COOKIE` | (trong code) | Cookie trình duyệt FB |
| `LISTEN` | `:8787` | Cổng HTTP server |
| `USE_PROXY` | `false` | Bật/tắt proxy ra Facebook |
| `PROXY_HOST` | m2proxy host:port | Gateway proxy |
| `PROXY_USER` | user:pass | Auth proxy (có thể kèm session) |
| `PROXY_FALLBACK_DIRECT` | `true` | Proxy fail → gọi thẳng |
| `TIMEOUT` | `12` | Timeout mỗi request FB (giây) |
| `RETRIES` | `0` | Số lần retry (gọi thẳng) |

Tool tìm `.env` theo: `./.env`, thư mục hiện tại, thư mục chứa **executable**.

---

## Flag dòng lệnh (ghi đè `.env`)

```text
-listen :8787
-proxy-host host:8080
-proxy-user user:pass
-proxy-pool 200
-no-proxy          # Bật flag = tắt proxy
-timeout 12
-retries 0
```

Ví dụ:

```bash
./tool_facebook -listen :9000 -no-proxy
```

---

## Chạy nền trên Linux (systemd) — gợi ý

Tạo `/etc/systemd/system/tool-facebook.service`:

```ini
[Unit]
Description=GetID Facebook API
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/opt/tool_facebook
ExecStart=/opt/tool_facebook/tool_facebook
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now tool-facebook
sudo systemctl status tool-facebook
```

Đặt binary + `.env` trong `/opt/tool_facebook/`.

---

## Firewall / reverse proxy

- Mở cổng `LISTEN` (vd. `8787`) hoặc chỉ expose qua **Nginx/Caddy** reverse proxy tới `127.0.0.1:8787`.
- Production nên dùng HTTPS phía trước (Caddy/Let's Encrypt).

---

## Xử lý lỗi thường gặp

| Triệu chứng | Gợi ý |
|-------------|--------|
| `proxyconnect ... :8080` | `USE_PROXY=true` nhưng không tới được proxy — kiểm tra mạng hoặc `USE_PROXY=false` |
| Trang web gọi sai cổng | Mở đúng `http://127.0.0.1:8787/` (không phải `:8080`) |
| `uid_not_found` | Cookie hết hạn, link riêng tư, hoặc HTML FB đổi — cập nhật `FB_COOKIE` |
| Build Linux trên Windows báo lỗi | Dùng `CGO_ENABLED=0` và đúng `GOOS`/`GOARCH` |
| Sửa HTML không thấy đổi | HTML embed trong binary — **build lại** và restart process |

---

## Tác giả

**DichVuRight** — API `author` trong JSON response.

---

## License

Dùng nội bộ / theo thỏa thuận team. Không commit `.env` chứa cookie thật lên git công khai.