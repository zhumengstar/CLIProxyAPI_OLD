#!/bin/sh
set -eu
LOG_DIR=${LOG_DIR:-/opt/CLIProxyAPI/logs}
MOUNT_PATH=${MOUNT_PATH:-/}
START_USAGE=${START_USAGE:-80}
TARGET_USAGE=${TARGET_USAGE:-70}
KEEP_RECENT_MINUTES=${KEEP_RECENT_MINUTES:-10}
REPORT=/var/log/cliproxy-cleanup/disk-pressure.log
mkdir -p "$(dirname "$REPORT")"
[ -d "$LOG_DIR" ] || exit 0
usage_pct() { df -P "$MOUNT_PATH" | awk 'NR==2 {gsub(/%/, "", $5); print $5}'; }
avail_kb() { df -P "$MOUNT_PATH" | awk 'NR==2 {print $4}'; }
usage=$(usage_pct)
if [ "$usage" -le "$START_USAGE" ]; then
  exit 0
fi
printf '%s start usage=%s%% avail_kb=%s log_size=%s\n' "$(date -Is)" "$usage" "$(avail_kb)" "$(du -sh "$LOG_DIR" 2>/dev/null | awk '{print $1}')" >> "$REPORT"
cutoff=$(date -d "$KEEP_RECENT_MINUTES minutes ago" +%s 2>/dev/null || date +%s)
find "$LOG_DIR" -maxdepth 1 -type f -name "*.log" ! -name "main.log" -printf '%T@ %p\n' 2>/dev/null | sort -n | while read -r ts path; do
  usage=$(usage_pct)
  if [ "$usage" -le "$TARGET_USAGE" ]; then break; fi
  ts_int=${ts%.*}
  if [ "$ts_int" -gt "$cutoff" ] && [ "$usage" -lt 95 ]; then continue; fi
  size=$(du -h "$path" 2>/dev/null | awk '{print $1}')
  rm -f -- "$path" && printf '%s deleted file size=%s path=%s\n' "$(date -Is)" "$size" "$path" >> "$REPORT"
done
find "$LOG_DIR" -maxdepth 1 -type d -name "request-log-parts-*" -printf '%T@ %p\n' 2>/dev/null | sort -n | while read -r ts path; do
  usage=$(usage_pct)
  if [ "$usage" -le "$TARGET_USAGE" ]; then break; fi
  ts_int=${ts%.*}
  if [ "$ts_int" -gt "$cutoff" ] && [ "$usage" -lt 95 ]; then continue; fi
  size=$(du -sh "$path" 2>/dev/null | awk '{print $1}')
  rm -rf -- "$path" && printf '%s deleted dir size=%s path=%s\n' "$(date -Is)" "$size" "$path" >> "$REPORT"
done
printf '%s finish usage=%s%% avail_kb=%s log_size=%s\n' "$(date -Is)" "$(usage_pct)" "$(avail_kb)" "$(du -sh "$LOG_DIR" 2>/dev/null | awk '{print $1}')" >> "$REPORT"
