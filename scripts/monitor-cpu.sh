#!/bin/sh
set -eu
OUT=/var/log/cliproxy-cleanup/cpu-monitor.log
mkdir -p "$(dirname "$OUT")"
{
  echo "=== $(date -Is) ==="
  uptime
  docker stats --no-stream --format '{{.Name}} CPU={{.CPUPerc}} MEM={{.MemUsage}} NET={{.NetIO}} BLOCK={{.BlockIO}}' cli-proxy-api new-api postgres redis entry-nginx 2>/dev/null || true
  ps -eo pid,comm,%cpu,%mem,etime,args --sort=-%cpu | head -12
} >> "$OUT"
lines=$(wc -l < "$OUT" 2>/dev/null || echo 0)
if [ "$lines" -gt 5000 ]; then
  tail -3000 "$OUT" > "$OUT.tmp" && mv "$OUT.tmp" "$OUT"
fi
