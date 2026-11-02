#!/bin/sh
# Notifier for statusd: called as `notify "message"`.
# Uses the first backend that is configured; add a block here for another service.
msg=${1:?usage: notify "message"}

if [ -n "$NTFY_URL" ]; then
    if [ -n "$NTFY_TOKEN" ]; then
        exec curl -fsS -m 10 -H "Authorization: Bearer $NTFY_TOKEN" -d "$msg" "$NTFY_URL"
    fi
    exec curl -fsS -m 10 -d "$msg" "$NTFY_URL"
fi

if [ -n "$TELEGRAM_BOT_TOKEN" ] && [ -n "$TELEGRAM_CHAT_ID" ]; then
    exec curl -fsS -m 10 -X POST \
        -d "chat_id=$TELEGRAM_CHAT_ID" --data-urlencode "text=$msg" \
        "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/sendMessage"
fi

if [ -n "$GOTIFY_URL" ] && [ -n "$GOTIFY_TOKEN" ]; then
    exec curl -fsS -m 10 -X POST -H "X-Gotify-Key: $GOTIFY_TOKEN" \
        -d "{\"title\":\"qwdtt-socks\",\"message\":\"$msg\"}" "$GOTIFY_URL/message"
fi

echo "notify: no backend configured (set NTFY_URL, or TELEGRAM_*, or GOTIFY_*)" >&2
exit 1
