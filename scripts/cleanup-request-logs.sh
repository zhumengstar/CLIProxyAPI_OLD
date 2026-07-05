#!/bin/sh
set -eu
LOG_DIR=${LOG_DIR:-/opt/CLIProxyAPI/logs}
TODAY=$(date +%F)
REPORT=/var/log/cliproxy-cleanup/request-cleanup.log
mkdir -p "$(dirname "$REPORT")"
[ -d "$LOG_DIR" ] || exit 0
before=$(du -sh "$LOG_DIR" 2>/dev/null | awk '{print $1}')
find "$LOG_DIR" -maxdepth 1 -type f -name "*.log" ! -name "main.log" ! -name "*$TODAY*" -print -delete >> "$REPORT" 2>&1 || true
find "$LOG_DIR" -maxdepth 1 -type d -name "request-log-parts-*" ! -newermt "$TODAY 00:00:00" -print -exec rm -rf {} + >> "$REPORT" 2>&1 || true
after=$(du -sh "$LOG_DIR" 2>/dev/null | awk '{print $1}')
printf '%s cleanup-request-logs before=%s after=%s\n' "$(date -Is)" "$before" "$after" >> "$REPORT"
