#!/bin/bash
# ============================================================================
# Subtitles King — Health & Status Snapshot
# ============================================================================
# Run anytime to see if the service is healthy. Read-only — safe to spam.
#
#   ./status.sh           Default summary
#   ./status.sh doctor    Deep deployment health check (cwd, toolchain, code
#                         drift, files/ writability, DB schema, MCP probe)
#   ./status.sh logs      Tail journal (follow mode)
#   ./status.sh errors    Show recent error lines from journal
#   ./status.sh queue     Show current pipeline state from videos.db
#   ./status.sh backups   List recent backups
#   ./status.sh rollback  Restore from the most recent backup (asks first)
# ============================================================================

set -euo pipefail

# Set SERVER_HOST + DOMAIN to your own VPS before running. Override at
# invocation:  SERVER_HOST=1.2.3.4 DOMAIN=mcp.example.com ./status.sh
SERVER_USER="${SERVER_USER:-root}"
SERVER_HOST="${SERVER_HOST:-CHANGE_ME.example.com}"
APP_NAME="${APP_NAME:-subtitlesking}"
REMOTE_DIR="${REMOTE_DIR:-/root/${APP_NAME}}"
PORT="${PORT:-8080}"
DOMAIN="${DOMAIN:-CHANGE_ME.example.com}"

case "${SERVER_HOST}${DOMAIN}" in
  *CHANGE_ME*)
    echo "✗ Set SERVER_HOST and DOMAIN before running status.sh."
    echo "  Either edit the defaults at the top of this script, or override at"
    echo "  invocation time:"
    echo "    SERVER_HOST=1.2.3.4 DOMAIN=mcp.example.com ./status.sh"
    exit 1
    ;;
esac

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'

cmd="${1:-status}"

case "$cmd" in
  logs)
    exec ssh "$SERVER_USER@$SERVER_HOST" "journalctl -u $APP_NAME -f --no-pager"
    ;;

  errors)
    ssh "$SERVER_USER@$SERVER_HOST" \
      "journalctl -u $APP_NAME -n 500 --no-pager -o cat 2>/dev/null \
        | grep -iE 'error|fail|panic|exit status|cannot|denied' \
        | tail -30 || echo '  (no recent errors)'"
    ;;

  doctor)
    echo -e "${BLUE}═══ Deployment doctor — deep health check ═══${NC}"
    echo

    # ── Code version drift ────────────────────────────────────────────────
    echo -e "${BLUE}▸ Deployed code version${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      cd ${REMOTE_DIR}
      if [ -d .git ]; then
        echo \"  git HEAD:    \$(git rev-parse --short HEAD 2>/dev/null) \$(git log -1 --pretty=%s 2>/dev/null)\"
        DIRTY=\$(git status --porcelain 2>/dev/null | wc -l)
        [ \"\$DIRTY\" -gt 0 ] && echo \"  git tree:    DIRTY (\$DIRTY uncommitted changes — risk of drift)\" || echo \"  git tree:    clean\"
      else
        echo '  (not a git checkout — cannot verify version)'
      fi
      echo \"  main.go:     mtime \$(stat -c '%y' main.go 2>/dev/null | cut -d. -f1)\"
      echo \"  binary:      mtime \$(stat -c '%y' ${APP_NAME} 2>/dev/null | cut -d. -f1 || echo 'no compiled binary at REMOTE_DIR')\"
    "
    echo

    # ── Code presence check (does the deployed source contain the new
    #    redesign symbols?) — catches partial/stale deploys ──────────────
    echo -e "${BLUE}▸ Code presence (new MCP redesign symbols)${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      cd ${REMOTE_DIR}
      check() {
        local label=\"\$1\" file=\"\$2\" pattern=\"\$3\"
        if grep -q \"\$pattern\" \"\$file\" 2>/dev/null; then
          echo \"  ✓ \$label\"
        else
          echo \"  ✗ \$label  ← MISSING in \$file\"
        fi
      }
      check 'start_upload tool'        src/server/mcp.go    'mcpStartUpload'
      check 'get_download_url tool'    src/server/mcp.go    'mcpGetDownloadURL'
      check 'pending_upload status'    src/server/mcp.go    'pending_upload'
      check 'bound-upload handler'     src/server/main.go   'markBoundUploadReady'
      check 'authToken→token binding'  src/server/main.go   'presignedTokenEntry'
      check 'pending_upload GC'        main.go              'cleanupPendingUploads'
      check 'compress stderr forward'  src/compress/main.go 'cmd.Stderr = os.Stderr'
    "
    echo

    # ── Daemon process state (where is it actually running from?) ──────
    echo -e "${BLUE}▸ Daemon process${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      PID=\$(systemctl show -p MainPID --value ${APP_NAME}.service 2>/dev/null)
      if [ -z \"\$PID\" ] || [ \"\$PID\" = '0' ]; then
        echo '  no MainPID — service is not running'
      else
        echo \"  pid:         \$PID\"
        echo \"  cwd:         \$(readlink /proc/\$PID/cwd 2>/dev/null || echo '?')\"
        echo \"  exe:         \$(readlink /proc/\$PID/exe 2>/dev/null || echo '?')\"
        START=\$(stat -c '%y' /proc/\$PID 2>/dev/null | cut -d. -f1)
        echo \"  started:     \$START\"
      fi
    "
    echo

    # ── Toolchain — go run uses these subprocesses every job ───────────
    echo -e "${BLUE}▸ Toolchain${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      probe() {
        local bin=\"\$1\" extra=\"\$2\"
        if command -v \"\$bin\" >/dev/null 2>&1; then
          local v=\$(\$bin \$extra 2>&1 | head -1)
          echo \"  ✓ \$bin: \$v\"
        else
          echo \"  ✗ \$bin: NOT FOUND on PATH\"
        fi
      }
      probe go      version
      probe ffmpeg  -version
      probe ffprobe -version
      probe sqlite3 --version
      if [ -f ${REMOTE_DIR}/sk/bin/python3 ]; then
        echo \"  ✓ python venv (sk/): \$(${REMOTE_DIR}/sk/bin/python3 --version 2>&1)\"
        if ${REMOTE_DIR}/sk/bin/python3 -c 'import whisper' 2>/dev/null; then
          echo '  ✓ whisper:    importable in venv'
        else
          echo '  ✗ whisper:    NOT importable in venv'
        fi
      else
        echo '  ✗ python venv (sk/): not found at REMOTE_DIR/sk'
      fi
    "
    echo

    # ── Filesystem layout (where uploads actually land) ──────────────
    echo -e "${BLUE}▸ Filesystem${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      DIR=${REMOTE_DIR}/files
      if [ -d \"\$DIR\" ]; then
        OWNER=\$(stat -c '%U:%G %a' \"\$DIR\" 2>/dev/null)
        WRITABLE=\$([ -w \"\$DIR\" ] && echo yes || echo NO)
        COUNT=\$(find \"\$DIR\" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)
        SIZE=\$(du -sh \"\$DIR\" 2>/dev/null | awk '{print \$1}')
        echo \"  files/:      \$DIR  (owner \$OWNER, writable=\$WRITABLE)\"
        echo \"  subdirs:     \$COUNT video directories, total \$SIZE\"
        # Most recent activity in files/
        RECENT=\$(find \"\$DIR\" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -3 | awk '{print \$2}')
        if [ -n \"\$RECENT\" ]; then
          echo '  newest:'
          echo \"\$RECENT\" | sed 's/^/    /'
        fi
      else
        echo \"  ✗ files/ directory does not exist at \$DIR\"
      fi
      df -h / | awk 'NR==2 {print \"  rootfs free: \" \$4 \" of \" \$2 \" (\" \$5 \" used)\"}'
    "
    echo

    # ── Database (schema, row distribution, recent errors) ─────────
    echo -e "${BLUE}▸ Database${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      DB=${REMOTE_DIR}/videos.db
      if [ ! -f \"\$DB\" ]; then
        echo \"  ✗ \$DB does not exist\"
      else
        echo \"  path:        \$DB\"
        echo '  schema columns:'
        sqlite3 \"\$DB\" 'PRAGMA table_info(videos);' | awk -F'|' '{printf \"    %s (%s)\n\", \$2, \$3}'
        echo '  rows by status:'
        sqlite3 \"\$DB\" 'SELECT status, COUNT(*) FROM videos GROUP BY status ORDER BY 2 DESC;' \
          | awk -F'|' '{printf \"    %-25s %s\n\", \$1, \$2}'
        echo '  most recent errors (5):'
        sqlite3 \"\$DB\" \"SELECT video_id||'  '||status||'  '||created_at FROM videos WHERE status LIKE 'error_%' ORDER BY created_at DESC LIMIT 5;\" \
          | sed 's/^/    /' || echo '    (none)'
      fi
    "
    echo

    # ── Network — local + public /mcp probe with new tool list ──────
    echo -e "${BLUE}▸ MCP endpoint${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      printf '  local /mcp (GET):  '
      curl -s -o /dev/null -w 'HTTP %{http_code}\n' http://localhost:${PORT}/mcp
      printf '  local tools/list:  '
      RESP=\$(curl -s -X POST http://localhost:${PORT}/mcp \
        -H 'Content-Type: application/json' \
        -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}')
      echo \"\$RESP\" | grep -oE '\"name\":\"[a-z_]+\"' | sed 's/\"name\":\"//;s/\"//' | tr '\n' ' '
      echo
    "
    PUBLIC=$(curl -s -o /dev/null -w "%{http_code}" "https://${DOMAIN}/mcp" 2>/dev/null || echo "000")
    if [ "$PUBLIC" = "200" ]; then
      echo -e "  public /mcp:       ${GREEN}HTTP 200${NC}"
    else
      echo -e "  public /mcp:       ${RED}HTTP $PUBLIC${NC}"
    fi
    echo

    # ── nginx (sits in front of the daemon) ───────────────────────────
    echo -e "${BLUE}▸ nginx${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      systemctl is-active nginx 2>/dev/null | sed 's/^/  systemd:     /'
      nginx -t 2>&1 | sed 's/^/  config:      /'
    "
    echo

    # ── Recent errors from the journal ────────────────────────────────
    echo -e "${BLUE}▸ Recent errors in journal${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      journalctl -u ${APP_NAME} -n 200 --no-pager -o cat 2>/dev/null \
        | grep -iE 'error|fail|panic|exit status' \
        | tail -8 \
        | sed 's/^/  /'
    " || echo "  (no recent errors)"
    echo

    echo -e "${BLUE}Done.${NC} Any line marked ✗ above is worth investigating."
    ;;

  queue)
    ssh "$SERVER_USER@$SERVER_HOST" \
      "sqlite3 -header -column $REMOTE_DIR/videos.db \"
        SELECT status, COUNT(*) AS count
        FROM videos
        GROUP BY status
        ORDER BY count DESC;
      \" && echo && echo 'In-flight (newest 10):' && \
       sqlite3 -header -column $REMOTE_DIR/videos.db \"
        SELECT video_id, file, status, created_at
        FROM videos
        WHERE status NOT IN ('subtitles_burned','deleted')
          AND status NOT LIKE 'error_%'
        ORDER BY created_at DESC LIMIT 10;
      \""
    ;;

  backups)
    ssh "$SERVER_USER@$SERVER_HOST" \
      "ls -lh /root/backups/${APP_NAME}/ 2>/dev/null | tail -10 || echo 'No backups yet.'"
    ;;

  rollback)
    echo -e "${YELLOW}Rollback restores the most recent backup's binary + systemd unit.${NC}"
    echo -e "${YELLOW}This will NOT restore the database (we keep current data).${NC}"
    read -rp "Continue? (y/N) " ans
    [[ "$ans" =~ ^[Yy]$ ]] || { echo "Aborted."; exit 0; }

    ssh "$SERVER_USER@$SERVER_HOST" "bash -s" <<EOF
set -e
LATEST=\$(ls -1dt /root/backups/${APP_NAME}/*/ 2>/dev/null | head -1)
[ -z "\$LATEST" ] && { echo "No backups found"; exit 1; }
echo "Rolling back from \$LATEST"

systemctl stop ${APP_NAME}.service 2>/dev/null || true

[ -f "\${LATEST}${APP_NAME}.deployed" ] && cp "\${LATEST}${APP_NAME}.deployed" "${REMOTE_DIR}/${APP_NAME}" \
  || { [ -f "\${LATEST}${APP_NAME}" ] && cp "\${LATEST}${APP_NAME}" "${REMOTE_DIR}/${APP_NAME}"; }

[ -f "\${LATEST}${APP_NAME}.service" ] && {
  cp "\${LATEST}${APP_NAME}.service" "/etc/systemd/system/${APP_NAME}.service"
  systemctl daemon-reload
}

systemctl start ${APP_NAME}.service
sleep 3
systemctl is-active --quiet ${APP_NAME}.service && echo "Rolled back successfully" || echo "Rollback issue — check journalctl"
EOF
    ;;

  status|*)
    echo -e "${BLUE}═══ Subtitles King status ═══${NC}"
    echo

    # systemd state
    echo -e "${BLUE}▸ Service${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      systemctl is-active ${APP_NAME}.service && echo '  active' || echo '  INACTIVE'
      systemctl show ${APP_NAME}.service --property=ActiveEnterTimestamp,NRestarts,MainPID --no-pager | sed 's/^/  /'
    "
    echo

    # Port
    echo -e "${BLUE}▸ Port ${PORT}${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      OWNER=\$(ss -tlnp 'sport = :${PORT}' 2>/dev/null | grep -v State | head -3)
      [ -n \"\$OWNER\" ] && echo \"\$OWNER\" | sed 's/^/  /' || echo '  (not bound)'
    "
    echo

    # Local /mcp probe
    echo -e "${BLUE}▸ Local /mcp probe${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      curl -s -o /dev/null -w '  http://localhost:${PORT}/mcp → HTTP %{http_code}\n' http://localhost:${PORT}/mcp
    "

    # Public probe
    PUBLIC=$(curl -s -o /dev/null -w "%{http_code}" "https://${DOMAIN}/mcp" 2>/dev/null || echo "000")
    if [ "$PUBLIC" = "200" ]; then
      echo -e "  ${GREEN}https://${DOMAIN}/mcp → HTTP 200${NC}"
    else
      echo -e "  ${RED}https://${DOMAIN}/mcp → HTTP $PUBLIC${NC}"
    fi
    echo

    # Queue summary
    echo -e "${BLUE}▸ Queue${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" \
      "sqlite3 ${REMOTE_DIR}/videos.db \"
         SELECT status, COUNT(*) FROM videos GROUP BY status ORDER BY 2 DESC;
       \" 2>/dev/null | awk -F'|' '{printf \"  %-25s %s\n\", \$1, \$2}'" \
       || echo "  (cannot read videos.db)"
    echo

    # Disk usage
    echo -e "${BLUE}▸ Disk${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" "
      du -sh ${REMOTE_DIR}/files 2>/dev/null | awk '{print \"  files/   \" \$1}'
      du -sh ${REMOTE_DIR}/sk    2>/dev/null | awk '{print \"  sk/      \" \$1}'
      du -sh ${REMOTE_DIR}/videos.db 2>/dev/null | awk '{print \"  videos.db \" \$1}'
      df -h / | awk 'NR==2 {print \"  rootfs   \" \$3 \" used / \" \$2 \" total (\" \$5 \" used)\"}'
    "
    echo

    # Recent log
    echo -e "${BLUE}▸ Last 8 log lines${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" \
      "journalctl -u ${APP_NAME} -n 8 --no-pager -o cat 2>/dev/null | sed 's/^/  /'"
    echo

    # Recent backups
    echo -e "${BLUE}▸ Recent backups${NC}"
    ssh "$SERVER_USER@$SERVER_HOST" \
      "ls -1dt /root/backups/${APP_NAME}/*/ 2>/dev/null | head -3 | sed 's/^/  /' || echo '  (none)'"
    echo

    echo -e "${BLUE}Tip:${NC} ./status.sh logs | queue | backups | rollback"
    ;;
esac
