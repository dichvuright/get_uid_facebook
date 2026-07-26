# tool_facebook — Linux / Ubuntu binary

Binary này build từ `main.go` + `static.go` cho Linux.

## Chọn đúng file theo server của anh

| File | Kiến trúc | Dùng khi... |
|---|---|---|
| `tool_facebookv_linux_amd64` | x86_64 | Ubuntu 22.04/24.04 trên PC, VPS thông dụng (Intel/AMD) |
| `tool_facebookv_linux_arm64` | aarch64 | Ubuntu 22.04/24.04 trên AWS Graviton, Raspberry Pi 4/5 (64-bit), Apple Silicon chạy Asahi |

Kiểm tra nhanh kiến trúc server:
```bash
uname -m
# x86_64  -> amd64
# aarch64 -> arm64
```

## Cài đặt (3 bước)

```bash
# 1. Upload binary lên server (chạy trên máy Windows)
scp tool_facebookv_linux_amd64 user@server:/opt/tool_facebook/

# 2. SSH vào server, chmod + chạy
ssh user@server
cd /opt/tool_facebook
chmod +x tool_facebookv_linux_amd64

# 3. Tạo .env (copy từ .env.example trong repo) và chạy
cp .env.example .env
nano .env                  # điền FB_COOKIE, PROXY_HOST, PROXY_USER
./tool_facebookv_linux_amd64
```

## Chạy nền với systemd (khuyến nghị)

Tạo file `/etc/systemd/system/tool_facebook.service`:
```ini
[Unit]
Description=tool_facebook - GetUID Facebook service
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/opt/tool_facebook
ExecStart=/opt/tool_facebook/tool_facebookv_linux_amd64
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now tool_facebook
sudo systemctl status tool_facebook
sudo journalctl -u tool_facebook -f    # xem log realtime
```

## Kiểm tra nhanh sau khi chạy

```bash
# Health check
curl -s http://127.0.0.1:8787/health

# Lấy UID từ username
curl -s "http://127.0.0.1:8787/api/v1/facebook?url=dichvuright.max"

# Debug (xem HTML Facebook trả về)
curl -s "http://127.0.0.1:8787/debug/raw?url=dichvuright.max"
```

## Checksums (SHA256)

```
9fd93d2c5bbbc2db1b54276ce24418ff25b75c9da7d9ab5510dcd6c444dd9a42  tool_facebookv_linux_amd64
14046baa00c2cf7ce1bba488d8df4a58ed250e52aed73b03adb5606f3a1cd208  tool_facebookv_linux_arm64
```

Verify:
```bash
sha256sum -c checksums.txt
```