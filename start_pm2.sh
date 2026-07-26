#!/usr/bin/env bash
# start_pm2.sh — khởi động tool_facebook qua pm2 trên Ubuntu/Linux
# Chạy:  bash start_pm2.sh            (mặc định listen :8787, name tool-facebook)
#        LISTEN=":9090" NAME="myapp" bash start_pm2.sh

set -e

APP_NAME="${NAME:-tool-facebook}"
LISTEN="${LISTEN:-0.0.0.0:8787}"
BIN_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$BIN_DIR/tool_facebook"
ENV_FILE="$BIN_DIR/.env"
LOG_DIR="$HOME/.pm2/logs"

echo "============================================================"
echo " tool_facebook — pm2 launcher"
echo "============================================================"
echo " APP_NAME  : $APP_NAME"
echo " LISTEN    : $LISTEN"
echo " BIN       : $BIN"
echo " ENV_FILE  : $ENV_FILE"
echo

# 1. Kiểm tra binary
if [ ! -f "$BIN" ]; then
    echo "✗ Không thấy file $BIN"
    echo "  → Tải binary mới: wget -O tool_facebook https://github.com/dichvuright/get_uid_facebook/raw/main/tool_facebook_linux_amd64"
    exit 1
fi
if [ ! -x "$BIN" ]; then
    echo "✗ Không thấy binary $BIN (hoặc chưa chmod +x)"
    echo "  → chmod +x tool_facebook && bash $0"
    exit 1
fi

# 1b. Kiểm tra binary có phải ELF Linux không (tránh 'cannot execute binary file')
FILE_TYPE=$(file -b "$BIN" 2>/dev/null || echo "")
case "$FILE_TYPE" in
    *"ELF"*"x86-64"*)         echo "✓ Binary: ELF x86_64 (amd64)" ;;
    *"ELF"*"aarch64"*)        echo "✓ Binary: ELF aarch64 (arm64)" ;;
    *"ELF"*"ARM"*)            echo "✓ Binary: ELF ARM" ;;
    *"ELF"*)                  echo "✓ Binary: ELF ($(uname -m))" ;;
    *"PE32"*"executable"*|*"MS Windows"*)
        echo "✗ Binary này là WINDOWS (.exe), không chạy được trên Linux!"
        echo "  → wget -O $BIN https://github.com/dichvuright/get_uid_facebook/raw/main/tool_facebook_linux_amd64"
        exit 1
        ;;
    *"HTML"*)
        echo "✗ File này là HTML (có thể tải nhầm trang 404 của GitHub)"
        echo "  → wget -O $BIN https://github.com/dichvuright/get_uid_facebook/raw/main/tool_facebook_linux_amd64"
        exit 1
        ;;
    *"gzip"*)
        echo "✗ File này là gzip (có thể bị nén / tải nhầm)"
        echo "  → wget -O $BIN https://github.com/dichvuright/get_uid_facebook/raw/main/tool_facebook_linux_amd64"
        exit 1
        ;;
    *)
        echo "✗ Binary không nhận diện được (file báo: '$FILE_TYPE')"
        echo "  → Kiểm tra lại: file $BIN"
        exit 1
        ;;
esac

# 2. Khởi tạo pm2 (nếu chưa có) + tạo thư mục log
mkdir -p "$LOG_DIR"
pm2 ping >/dev/null 2>&1 || pm2 init >/dev/null

# 3. Nếu app đã tồn tại → delete để start sạch
if pm2 describe "$APP_NAME" >/dev/null 2>&1; then
    echo ">> Đang dừng + xóa app cũ: $APP_NAME"
    pm2 delete "$APP_NAME" >/dev/null 2>&1 || true
fi

# 4. Start binary với LISTEN trong env (binary sẽ đọc LISTEN, fallback default)
echo ">> Đang start: $BIN (LISTEN=$LISTEN)"
pm2 start "$BIN" \
    --name "$APP_NAME" \
    --cwd "$BIN_DIR" \
    --time \
    --merge-logs \
    --log-date-format "YYYY-MM-DD HH:mm:ss Z" \
    --env "LISTEN=$LISTEN"

# 5. Lưu lại để pm2 resurrect khi reboot
pm2 save >/dev/null

echo
echo ">> Trạng thái:"
pm2 list

echo
echo ">> 30 log mới nhất của $APP_NAME:"
pm2 log "$APP_NAME" --lines 30 --nostream --raw 2>/dev/null || pm2 logs "$APP_NAME" --lines 30 --nostream

echo
echo "============================================================"
echo " Test kết nối:"
echo "   curl http://127.0.0.1:${LISTEN##*:}/health"
echo "   curl 'http://127.0.0.1:${LISTEN##*:}/api/v1/facebook?url=dichvuright.max'"
echo "============================================================"
echo " Quản lý:"
echo "   pm2 log $APP_NAME          # xem log realtime"
echo "   pm2 restart $APP_NAME      # restart"
echo "   pm2 reload $APP_NAME       # graceful reload (0-downtime)"
echo "   pm2 stop $APP_NAME         # dừng"
echo "   pm2 delete $APP_NAME      # xóa khỏi pm2"
echo "============================================================"