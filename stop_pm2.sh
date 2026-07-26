#!/usr/bin/env bash
# stop_pm2.sh — dừng + xóa app tool_facebook khỏi pm2
set -e

APP_NAME="${NAME:-tool-facebook}"

if ! pm2 describe "$APP_NAME" >/dev/null 2>&1; then
    echo ">> Không có app '$APP_NAME' trong pm2 — không làm gì."
    exit 0
fi

echo ">> Đang dừng + xóa: $APP_NAME"
pm2 delete "$APP_NAME"
pm2 save >/dev/null
echo ">> Xong."