#!/bin/bash
# ============================================================================
# Subtitles King — Backend Deploy Script
# ============================================================================
# Two-stage deploy (local orchestrator + server-side runner) following the
# pattern in /Users/kz/dev/hetzner-manager/DEPLOY-SCRIPT-GUIDE.md.
#
# What this does on every run:
#   1. Local preflight  — SSH, git cleanliness, port-owner check
#   2. Build locally    — catches compile errors before touching prod
#   3. Backup           — videos.db copy + SQLite integrity check
#   4. Sync source      — rsync with explicit excludes for sk/, files/, db
#   5. Auto-heal        — installs missing apt packages (ffmpeg, python3-venv,
#                          sqlite3, lsof), creates files/ if missing, and
#                          bootstraps the Python venv + Whisper if absent
#                          (first-time bootstrap takes ~5 min and ~5 GB)
#   6. Server build     — compile main + MCP binaries on the box (CGO needs
#                          to match the target's libc; build on target)
#   7. Switch to binary — replace the brittle `go run` start.sh with a
#                          compiled binary + cgroup-aware systemd unit
#   8. Restart + verify — log check, /mcp probe, auto-rollback on failure
#
# What this PRESERVES on the server (never overwritten):
#   /root/subtitlesking/sk/                 Python venv (Whisper)
#   /root/subtitlesking/videos.db           SQLite metadata
#   /root/subtitlesking/files/              In-flight video processing
#   /root/subtitlesking/letsencrypt/        SSL certs (mirror of /etc/letsencrypt)
#
# Rollback:
#   On any failure, the script restores the previous binary + systemd unit
#   from /root/backups/subtitlesking/$TIMESTAMP/ and restarts the service.
# ============================================================================

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# Config
# ──────────────────────────────────────────────────────────────────────────────

APP_NAME="subtitlesking"
# Set SERVER_HOST + DOMAIN to your own VPS before running this script.
# Override at invocation time, e.g.
#   SERVER_HOST=1.2.3.4 DOMAIN=mcp.example.com ./deploy.sh
SERVER_USER="${SERVER_USER:-root}"
SERVER_HOST="${SERVER_HOST:-CHANGE_ME.example.com}"
REMOTE_DIR="${REMOTE_DIR:-/root/${APP_NAME}}"
PORT="${PORT:-8080}"
DOMAIN="${DOMAIN:-CHANGE_ME.example.com}"
LOCAL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Refuse to run with placeholder values (but allow override via env).
case "${SERVER_HOST}${DOMAIN}" in
  *CHANGE_ME*)
    echo "✗ Set SERVER_HOST and DOMAIN before running deploy.sh."
    echo "  Either edit the defaults at the top of this script, or override at"
    echo "  invocation time:"
    echo "    SERVER_HOST=1.2.3.4 DOMAIN=mcp.example.com ./deploy.sh"
    exit 1
    ;;
esac

# Knobs (env vars on the server, baked into the systemd unit on each deploy).
# Names are prefixed with SUBTITLESKING_ to avoid collision with other services
# on the shared VPS (md2doc, scoutzie, transcriptking, etc).
SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR="${SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR:-5}"
SUBTITLESKING_MCP_LIMIT_PER_HOUR="${SUBTITLESKING_MCP_LIMIT_PER_HOUR:-5}"
SUBTITLESKING_MCP_CONCURRENCY="${SUBTITLESKING_MCP_CONCURRENCY:-10}"
SUBTITLESKING_MAX_UPLOAD_BYTES="${SUBTITLESKING_MAX_UPLOAD_BYTES:-104857600}"  # 100 MB
SUBTITLESKING_MAX_WORKERS="${SUBTITLESKING_MAX_WORKERS:-2}"
# Default Whisper model when the row has no per-upload override (set by
# the MCP `quality` arg). medium is the speed/accuracy sweet spot on CPU
# — large is ~3× slower, base is ~5× faster but rougher.
SUBTITLESKING_WHISPER_MODEL="${SUBTITLESKING_WHISPER_MODEL:-medium}"

# Subtitle chunking. 42-char × 2-line is the broadcast-safe default that
# yields ~3–5 second segments at conversational speech rates. Set
# SUBTITLESKING_SRT_MAX_WORDS_PER_LINE to a small number (e.g. 4) for
# TikTok-style short bursts; that flag is mutually exclusive with the
# width/count pair in Whisper's CLI.
SUBTITLESKING_SRT_MAX_LINE_WIDTH="${SUBTITLESKING_SRT_MAX_LINE_WIDTH:-42}"
SUBTITLESKING_SRT_MAX_LINE_COUNT="${SUBTITLESKING_SRT_MAX_LINE_COUNT:-2}"
SUBTITLESKING_SRT_MAX_WORDS_PER_LINE="${SUBTITLESKING_SRT_MAX_WORDS_PER_LINE:-}"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'
YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'

step()   { echo -e "${BLUE}▸ $*${NC}"; }
ok()     { echo -e "${GREEN}✓ $*${NC}"; }
warn()   { echo -e "${YELLOW}⚠ $*${NC}"; }
fail()   { echo -e "${RED}✗ $*${NC}" >&2; exit 1; }

# Temp dirs (cleaned by trap)
TS=$(date +%Y%m%d_%H%M%S)
TEMP_DIR=$(mktemp -d)
REMOTE_TEMP_DIR="/tmp/${APP_NAME}_deploy_${TS}"

cleanup() {
  rm -rf "$TEMP_DIR" 2>/dev/null || true
  ssh -o ConnectTimeout=5 "$SERVER_USER@$SERVER_HOST" "rm -rf $REMOTE_TEMP_DIR" 2>/dev/null || true
}
trap cleanup EXIT

echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Subtitles King Deploy → ${SERVER_USER}@${SERVER_HOST}${NC}"
echo -e "${GREEN}  Timestamp: ${TS}${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"

# ──────────────────────────────────────────────────────────────────────────────
# 1. Local preflight
# ──────────────────────────────────────────────────────────────────────────────

step "Preflight: don't run as root locally"
[ "$(id -u)" = "0" ] && fail "Don't run this as root locally. It SSHs as root to the server."

step "Preflight: required local tools"
for bin in ssh scp rsync go tar; do
  command -v "$bin" >/dev/null || fail "Missing local tool: $bin"
done
ok "All local tools present"

step "Preflight: working dir is upload-server/canary"
[ -f "$LOCAL_DIR/main.go" ] || fail "main.go not found in $LOCAL_DIR — run from upload-server/canary/"
[ -d "$LOCAL_DIR/cmd/mcp-server" ] || fail "cmd/mcp-server/ missing — wrong directory?"

step "Preflight: warn if there are uncommitted changes"
if command -v git >/dev/null && git -C "$LOCAL_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ -n "$(git -C "$LOCAL_DIR" status --porcelain 2>/dev/null)" ]; then
    warn "You have uncommitted changes — they'll be deployed but not in git"
    git -C "$LOCAL_DIR" status --short | head -10
    read -rp "Continue anyway? (y/N) " ans
    [[ "$ans" =~ ^[Yy]$ ]] || fail "Aborted."
  else
    ok "Working tree clean"
  fi
fi

step "Preflight: local Go build (catches errors before touching prod)"
(cd "$LOCAL_DIR" && go build -o "$TEMP_DIR/local-build-test" . >/dev/null) \
  || fail "Local build failed — fix compile errors before deploying"
(cd "$LOCAL_DIR" && go build -o "$TEMP_DIR/local-mcp-test" ./cmd/mcp-server >/dev/null) \
  || fail "Local MCP binary build failed"
rm -f "$TEMP_DIR/local-build-test" "$TEMP_DIR/local-mcp-test"
ok "Local builds succeed"

step "Preflight: SSH connectivity"
ssh -o ConnectTimeout=5 -o BatchMode=yes "$SERVER_USER@$SERVER_HOST" "echo ok" >/dev/null \
  || fail "SSH to $SERVER_USER@$SERVER_HOST failed"
ok "SSH OK"

step "Preflight: confirm port $PORT is owned by us (not a stray process)"
PORT_OWNER=$(ssh "$SERVER_USER@$SERVER_HOST" "ss -tlnp 'sport = :$PORT' 2>/dev/null | grep -v 'State' || true")
echo "  Port $PORT: ${PORT_OWNER:-(free)}"
if [ -n "$PORT_OWNER" ] && ! echo "$PORT_OWNER" | grep -qE "(${APP_NAME}|go|start\.sh)"; then
  warn "Port $PORT is held by something that doesn't look like ${APP_NAME}"
  warn "If this is a rogue manual process, kill it first. If it's another service, DO NOT proceed."
  read -rp "Continue anyway? (y/N) " ans
  [[ "$ans" =~ ^[Yy]$ ]] || fail "Aborted."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 2. Server-side: backup + integrity check
# ──────────────────────────────────────────────────────────────────────────────

step "Backup videos.db on server (with integrity check)"
ssh "$SERVER_USER@$SERVER_HOST" "bash -s" <<EOF
set -e
BACKUP_DIR="/root/backups/${APP_NAME}/${TS}"
mkdir -p "\$BACKUP_DIR"

if [ -f "${REMOTE_DIR}/videos.db" ]; then
  cp "${REMOTE_DIR}/videos.db" "\$BACKUP_DIR/videos.db"
  CHECK=\$(sqlite3 "\$BACKUP_DIR/videos.db" "PRAGMA integrity_check;" 2>&1)
  if [ "\$CHECK" != "ok" ]; then
    echo "DB integrity check FAILED: \$CHECK"
    exit 1
  fi
  # A failed prior deploy may leave an empty videos.db with no schema (setup.go
  # never ran). Don't error on missing 'videos' table — count rows only if it exists.
  if sqlite3 "\$BACKUP_DIR/videos.db" "SELECT 1 FROM videos LIMIT 0" >/dev/null 2>&1; then
    ROW_COUNT=\$(sqlite3 "\$BACKUP_DIR/videos.db" "SELECT COUNT(*) FROM videos;")
    echo "DB backed up to \$BACKUP_DIR (rows: \$ROW_COUNT)"
  else
    echo "DB backed up to \$BACKUP_DIR (schema not yet initialized — likely from a prior failed deploy)"
  fi
else
  echo "No existing videos.db — fresh install"
fi

# Also snapshot the binary + systemd unit + start.sh for rollback
[ -f "${REMOTE_DIR}/${APP_NAME}" ] && cp "${REMOTE_DIR}/${APP_NAME}" "\$BACKUP_DIR/" || true
[ -f "${REMOTE_DIR}/start.sh" ] && cp "${REMOTE_DIR}/start.sh" "\$BACKUP_DIR/" || true
[ -f "/etc/systemd/system/${APP_NAME}.service" ] && cp "/etc/systemd/system/${APP_NAME}.service" "\$BACKUP_DIR/${APP_NAME}.service" || true
EOF
ok "Backup complete: /root/backups/${APP_NAME}/${TS}/"

# ──────────────────────────────────────────────────────────────────────────────
# 3. Sync source code (preserve sk/, files/, videos.db, letsencrypt/)
# ──────────────────────────────────────────────────────────────────────────────

step "Syncing source to server (preserving venv, db, files, certs)"
rsync -az --delete-after \
  --exclude='sk/' \
  --exclude='files/' \
  --exclude='videos.db' \
  --exclude='videos.db-journal' \
  --exclude='videos.db-wal' \
  --exclude='videos.db-shm' \
  --exclude='letsencrypt/' \
  --exclude='ssl/' \
  --exclude='.git/' \
  --exclude='.DS_Store' \
  --exclude='*.bak' \
  --exclude='deploy.log' \
  --exclude='subtitlesking' \
  --exclude='subtitlesking-mcp' \
  --exclude=':etc:nginx:sites-enabled:' \
  --exclude=':etc:systemd:system:' \
  --exclude='etc/' \
  "$LOCAL_DIR/" "$SERVER_USER@$SERVER_HOST:$REMOTE_DIR/"
ok "Source synced"

# ──────────────────────────────────────────────────────────────────────────────
# 4. Generate + run server-side deploy script
# ──────────────────────────────────────────────────────────────────────────────

step "Generating server-side deploy script"
cat > "$TEMP_DIR/server_deploy.sh" <<'SERVER_SCRIPT'
#!/bin/bash
set -euo pipefail

APP_NAME="__APP_NAME__"
BASE_DIR="__REMOTE_DIR__"
PORT="__PORT__"
DOMAIN="__DOMAIN__"
TS="__TS__"
SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR="__PRESIGN_LIMIT__"
SUBTITLESKING_MCP_LIMIT_PER_HOUR="__MCP_LIMIT__"
SUBTITLESKING_MCP_CONCURRENCY="__MCP_CONCURRENCY__"
SUBTITLESKING_MAX_UPLOAD_BYTES="__MAX_UPLOAD_BYTES__"
SUBTITLESKING_MAX_WORKERS="__MAX_WORKERS__"
SUBTITLESKING_WHISPER_MODEL="__WHISPER_MODEL__"
SUBTITLESKING_SRT_MAX_LINE_WIDTH="__SRT_MAX_LINE_WIDTH__"
SUBTITLESKING_SRT_MAX_LINE_COUNT="__SRT_MAX_LINE_COUNT__"
SUBTITLESKING_SRT_MAX_WORDS_PER_LINE="__SRT_MAX_WORDS_PER_LINE__"

BACKUP_DIR="/root/backups/${APP_NAME}/${TS}"
GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
say()  { echo -e "${BLUE}▸ $*${NC}"; }
ok()   { echo -e "${GREEN}✓ $*${NC}"; }
fail() { echo -e "${RED}✗ $*${NC}" >&2; exit 1; }

# ----- Locate Go (the systemd unit will need it on PATH for sub-pipeline runs)
GO_BIN=""
for p in /usr/local/go/bin/go /usr/bin/go; do
  [ -x "$p" ] && GO_BIN="$p" && break
done
[ -n "$GO_BIN" ] || fail "Go not found on server (install with: apt install golang-go OR https://go.dev/dl/)"
GO_PATH=$(dirname "$GO_BIN")
ok "Go: $GO_BIN"

# ----- Auto-heal: system packages the pipeline depends on.
# Installs only what's missing. Idempotent — fast no-op when already present.
say "Ensure system packages (ffmpeg, sqlite3, lsof, python3, python3-venv)"
NEED_APT=()
command -v ffmpeg  >/dev/null 2>&1 || NEED_APT+=(ffmpeg)
command -v ffprobe >/dev/null 2>&1 || NEED_APT+=(ffmpeg)
command -v sqlite3 >/dev/null 2>&1 || NEED_APT+=(sqlite3)
command -v lsof    >/dev/null 2>&1 || NEED_APT+=(lsof)
command -v python3 >/dev/null 2>&1 || NEED_APT+=(python3)
# python3-venv is a separate Debian package even when python3 is present.
python3 -c 'import ensurepip' >/dev/null 2>&1 || NEED_APT+=(python3-venv python3-pip)
# Dedupe.
if [ "${#NEED_APT[@]}" -gt 0 ]; then
  IFS=$'\n' NEED_APT=($(sort -u <<<"${NEED_APT[*]}")); unset IFS
  echo "  installing missing packages: ${NEED_APT[*]}"
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq \
      -o Dpkg::Options::=--force-confdef \
      -o Dpkg::Options::=--force-confold \
      "${NEED_APT[@]}" \
      || fail "apt-get install failed for: ${NEED_APT[*]}"
  else
    fail "Missing system packages and no apt-get available: ${NEED_APT[*]}"
  fi
fi
ok "System packages OK"

# ----- Auto-heal: files/ directory (uploads land here)
say "Ensure files/ directory"
mkdir -p "$BASE_DIR/files"
chmod 755 "$BASE_DIR/files"
ok "files/ ready at $BASE_DIR/files"

# ----- Auto-heal: Python venv + Whisper (was the most common deploy hazard).
# Three states we handle:
#   (a) venv present + whisper importable  → fast pass
#   (b) venv present but whisper missing   → repair via pip
#   (c) venv missing entirely              → bootstrap from scratch
# Bootstrap is slow (~5 min, ~5 GB disk for torch + whisper) but unattended.
say "Ensure Python venv (Whisper) at $BASE_DIR/sk"
venv_ready=false
if [ -d "$BASE_DIR/sk/bin" ] && "$BASE_DIR/sk/bin/python3" -c 'import whisper' >/dev/null 2>&1; then
  venv_ready=true
fi

if ! $venv_ready; then
  if [ -d "$BASE_DIR/sk/bin" ]; then
    echo "  venv present but whisper not importable — repairing"
  else
    echo "  venv missing — bootstrapping (this takes ~5 min, needs ~5 GB disk for openai-whisper + torch)"
    AVAIL_KB=$(df -P "$BASE_DIR" | awk 'NR==2 {print $4}')
    [ "$AVAIL_KB" -ge 8000000 ] \
      || fail "Need ~8 GB free at $BASE_DIR; only $((AVAIL_KB/1024)) MB available. Free up disk first."
    python3 -m venv "$BASE_DIR/sk" \
      || fail "venv creation failed (python3-venv apt package required)"
  fi

  "$BASE_DIR/sk/bin/pip" install --quiet --upgrade pip wheel 'setuptools<80' \
    || fail "pip self-upgrade failed"

  # openai-whisper 20240930's setup.py does `import pkg_resources`, which
  # setuptools 80+ no longer ships in pip's PEP 517 build-isolation env.
  # Constrain the build env's setuptools so the build can resolve pkg_resources.
  BUILD_CONSTRAINTS="$BASE_DIR/sk/build-constraints.txt"
  printf 'setuptools<80\nwheel\n' > "$BUILD_CONSTRAINTS"

  if [ -f "$BASE_DIR/requirements.txt" ]; then
    echo "  installing from requirements.txt (multi-GB download — torch + whisper)"
    PIP_CONSTRAINT="$BUILD_CONSTRAINTS" "$BASE_DIR/sk/bin/pip" install --quiet -r "$BASE_DIR/requirements.txt" \
      || fail "pip install from requirements.txt failed"
  else
    echo "  no requirements.txt found — falling back to plain openai-whisper"
    PIP_CONSTRAINT="$BUILD_CONSTRAINTS" "$BASE_DIR/sk/bin/pip" install --quiet openai-whisper \
      || fail "openai-whisper install failed"
  fi

  "$BASE_DIR/sk/bin/python3" -c 'import whisper' \
    || fail "whisper still not importable after install — investigate manually"
fi
ok "Python venv ready (whisper importable)"

# ----- Fix go.mod for server's Go version (defensive — script-guide pattern)
say "Normalize go.mod for server Go version"
cd "$BASE_DIR"
if [ -f go.mod ]; then
  cp go.mod go.mod.bak
  sed -i '/^toolchain/d' go.mod || true
fi

# ----- Build new binaries
say "Building $APP_NAME (CGO_ENABLED=1 for sqlite)"
export PATH="$GO_PATH:$PATH"
CGO_ENABLED=1 go build -o "${APP_NAME}.new" .
ok "Main binary built ($(du -h ${APP_NAME}.new | cut -f1))"

say "Building MCP stdio binary"
CGO_ENABLED=0 go build -o "${APP_NAME}-mcp.new" ./cmd/mcp-server
ok "MCP binary built"

# ----- Stop existing service (graceful)
say "Stop existing systemd service"
systemctl stop "${APP_NAME}.service" 2>/dev/null || true
sleep 2

# ----- Kill any lingering process on PORT (the rogue-process problem)
LINGER=$(lsof -t -i:${PORT} 2>/dev/null || true)
if [ -n "$LINGER" ]; then
  echo "  killing lingering PID(s) on port ${PORT}: $LINGER"
  kill $LINGER 2>/dev/null || true
  sleep 2
  STILL=$(lsof -t -i:${PORT} 2>/dev/null || true)
  [ -n "$STILL" ] && kill -9 $STILL 2>/dev/null || true
fi
ok "Port ${PORT} clear"

# ----- Atomically swap binaries
say "Swap in new binaries"
mv "${APP_NAME}.new" "${APP_NAME}"
mv "${APP_NAME}-mcp.new" "${APP_NAME}-mcp"
chmod +x "${APP_NAME}" "${APP_NAME}-mcp"

# ----- Write a fresh systemd unit (cgroup-aware so children die with parent)
say "Update systemd unit"
cat > "/etc/systemd/system/${APP_NAME}.service" <<EOF
[Unit]
Description=SubtitlesKing — video subtitle generation
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${BASE_DIR}
ExecStart=${BASE_DIR}/${APP_NAME}
Restart=always
RestartSec=10

# PATH includes the Whisper venv (so child processes can find \`whisper\`)
# and Go (for the in-pipeline \`go run\` subprocess invocations).
Environment=PATH=${BASE_DIR}/sk/bin:${GO_PATH}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=VIRTUAL_ENV=${BASE_DIR}/sk
Environment=PORT=${PORT}
Environment=SUBTITLESKING_ENV=production

# \`go run\` subprocesses (src/setup, src/compress, src/srt, src/burn) need
# HOME + GOPATH/GOMODCACHE; systemd doesn't auto-populate HOME for root services.
Environment=HOME=/root
Environment=GOPATH=/root/go
Environment=GOMODCACHE=/root/go/pkg/mod
Environment=GOCACHE=/root/.cache/go-build

# Whisper model cache is shared across services on this VPS (~5GB of .pt files).
# Without this, whisper defaults to ~/.cache/whisper and silently re-downloads
# every model — wasting ~5GB and duplicating what transcriptking already has.
Environment=XDG_CACHE_HOME=/root/cache/whisper

# Free-tier limits + worker pool (tune via redeploy). Prefix avoids clashes
# on the shared VPS — see DEPLOY-SCRIPT-GUIDE.md.
Environment=SUBTITLESKING_WHISPER_MODEL=${SUBTITLESKING_WHISPER_MODEL}
Environment=SUBTITLESKING_SRT_MAX_LINE_WIDTH=${SUBTITLESKING_SRT_MAX_LINE_WIDTH}
Environment=SUBTITLESKING_SRT_MAX_LINE_COUNT=${SUBTITLESKING_SRT_MAX_LINE_COUNT}
Environment=SUBTITLESKING_SRT_MAX_WORDS_PER_LINE=${SUBTITLESKING_SRT_MAX_WORDS_PER_LINE}
Environment=SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR=${SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR}
Environment=SUBTITLESKING_MCP_LIMIT_PER_HOUR=${SUBTITLESKING_MCP_LIMIT_PER_HOUR}
Environment=SUBTITLESKING_MCP_CONCURRENCY=${SUBTITLESKING_MCP_CONCURRENCY}
Environment=SUBTITLESKING_MAX_UPLOAD_BYTES=${SUBTITLESKING_MAX_UPLOAD_BYTES}
Environment=SUBTITLESKING_MAX_WORKERS=${SUBTITLESKING_MAX_WORKERS}

# Kill ALL processes in the cgroup on stop — fixes the rogue child-process
# bug where \`go run\` parent dies but the bound child holds port ${PORT}.
KillMode=control-group
KillSignal=SIGTERM
TimeoutStopSec=30

StandardOutput=journal
StandardError=journal
SyslogIdentifier=${APP_NAME}

[Install]
WantedBy=multi-user.target
EOF
ok "systemd unit written"

# ----- Reload + start
systemctl daemon-reload
systemctl enable "${APP_NAME}.service" >/dev/null
say "Starting service"
systemctl start "${APP_NAME}.service"

# ----- Verify
sleep 4
if ! systemctl is-active --quiet "${APP_NAME}.service"; then
  echo -e "${RED}✗ Service failed to start — recent logs:${NC}"
  journalctl -u "${APP_NAME}.service" -n 40 --no-pager
  fail "Service start failed"
fi
ok "Service active"

# ----- Post-start log markers
say "Verifying startup markers in log"
sleep 1
LOG=$(journalctl -u "${APP_NAME}.service" -n 30 --no-pager)
echo "$LOG" | grep -q "main: starting worker pool" \
  || fail "Did not see 'main: starting worker pool' in logs — old code may still be running"
echo "$LOG" | grep -q "Listening on :${PORT}" \
  || fail "Did not see Listening on :${PORT}"
ok "New code is running"

# ----- Local /mcp probe (does the HTTP MCP respond?)
say "Probing localhost:${PORT}/mcp"
PROBE=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:${PORT}/mcp" || echo "000")
[ "$PROBE" = "200" ] || fail "/mcp returned HTTP $PROBE (expected 200)"
ok "/mcp endpoint healthy"

# ----- Backup binary for rollback
cp "${BASE_DIR}/${APP_NAME}" "${BACKUP_DIR}/${APP_NAME}.deployed" 2>/dev/null || true

# ----- Prune old backups (keep most recent 5)
say "Pruning old backups (keeping 5 most recent)"
cd "/root/backups/${APP_NAME}" 2>/dev/null && ls -1dt */ 2>/dev/null | tail -n +6 | xargs rm -rf 2>/dev/null || true
ok "Old backups pruned"

echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Server-side deploy: SUCCESS${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════${NC}"
SERVER_SCRIPT

# Substitute placeholders
sed_inplace() {
  if sed --version >/dev/null 2>&1; then sed -i "$@"; else sed -i '' "$@"; fi
}
sed_inplace "s|__APP_NAME__|${APP_NAME}|g"               "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__REMOTE_DIR__|${REMOTE_DIR}|g"           "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__PORT__|${PORT}|g"                       "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__DOMAIN__|${DOMAIN}|g"                   "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__TS__|${TS}|g"                           "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__PRESIGN_LIMIT__|${SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR}|g" "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__MCP_LIMIT__|${SUBTITLESKING_MCP_LIMIT_PER_HOUR}|g"    "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__MCP_CONCURRENCY__|${SUBTITLESKING_MCP_CONCURRENCY}|g" "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__MAX_UPLOAD_BYTES__|${SUBTITLESKING_MAX_UPLOAD_BYTES}|g" "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__MAX_WORKERS__|${SUBTITLESKING_MAX_WORKERS}|g"         "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__WHISPER_MODEL__|${SUBTITLESKING_WHISPER_MODEL}|g"     "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__SRT_MAX_LINE_WIDTH__|${SUBTITLESKING_SRT_MAX_LINE_WIDTH}|g"   "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__SRT_MAX_LINE_COUNT__|${SUBTITLESKING_SRT_MAX_LINE_COUNT}|g"   "$TEMP_DIR/server_deploy.sh"
sed_inplace "s|__SRT_MAX_WORDS_PER_LINE__|${SUBTITLESKING_SRT_MAX_WORDS_PER_LINE}|g" "$TEMP_DIR/server_deploy.sh"

chmod +x "$TEMP_DIR/server_deploy.sh"

step "Uploading deploy runner"
ssh "$SERVER_USER@$SERVER_HOST" "mkdir -p $REMOTE_TEMP_DIR"
scp -q "$TEMP_DIR/server_deploy.sh" "$SERVER_USER@$SERVER_HOST:$REMOTE_TEMP_DIR/"

step "Running server-side deploy"
if ssh "$SERVER_USER@$SERVER_HOST" "$REMOTE_TEMP_DIR/server_deploy.sh"; then
  ok "Server-side deploy succeeded"
else
  warn "Server-side deploy FAILED — attempting automatic rollback"
  ssh "$SERVER_USER@$SERVER_HOST" "bash -s" <<EOF
set +e
BACKUP_DIR="/root/backups/${APP_NAME}/${TS}"
echo "Rolling back from \$BACKUP_DIR"

systemctl stop ${APP_NAME}.service 2>/dev/null

# Restore previous binary if we have one
if [ -f "\$BACKUP_DIR/${APP_NAME}" ]; then
  cp "\$BACKUP_DIR/${APP_NAME}" "${REMOTE_DIR}/${APP_NAME}"
  echo "Restored binary"
fi

# Restore previous systemd unit if we have one
if [ -f "\$BACKUP_DIR/${APP_NAME}.service" ]; then
  cp "\$BACKUP_DIR/${APP_NAME}.service" "/etc/systemd/system/${APP_NAME}.service"
  systemctl daemon-reload
  echo "Restored systemd unit"
fi

# Restore DB only if it's missing or broken (be conservative)
if [ -f "\$BACKUP_DIR/videos.db" ] && [ ! -f "${REMOTE_DIR}/videos.db" ]; then
  cp "\$BACKUP_DIR/videos.db" "${REMOTE_DIR}/videos.db"
  echo "Restored videos.db"
fi

systemctl start ${APP_NAME}.service 2>/dev/null
sleep 3
systemctl is-active --quiet ${APP_NAME}.service && echo "Rolled back, service active" || echo "Rollback may not have succeeded — investigate"
EOF
  fail "Deploy failed; rollback attempted. Check service status manually."
fi

# ──────────────────────────────────────────────────────────────────────────────
# 5. Public smoke test from local machine
# ──────────────────────────────────────────────────────────────────────────────

step "Public smoke test against https://${DOMAIN}"
sleep 2

PUBLIC_PROBE=$(curl -s -o /dev/null -w "%{http_code}" "https://${DOMAIN}/mcp" || echo "000")
if [ "$PUBLIC_PROBE" = "200" ]; then
  ok "https://${DOMAIN}/mcp returned 200"
else
  warn "https://${DOMAIN}/mcp returned $PUBLIC_PROBE — could be DNS, nginx, or cert"
fi

echo
echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Deploy complete — ${TS}${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════════${NC}"
echo -e "  Backup:  /root/backups/${APP_NAME}/${TS}/"
echo -e "  Logs:    ssh ${SERVER_USER}@${SERVER_HOST} journalctl -u ${APP_NAME} -f"
echo -e "  Status:  ssh ${SERVER_USER}@${SERVER_HOST} systemctl status ${APP_NAME}"
echo -e "  Probe:   curl https://${DOMAIN}/mcp"
echo
echo -e "  Tune limits via env vars on next deploy:"
echo -e "    SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR=${SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR}"
echo -e "    SUBTITLESKING_MCP_LIMIT_PER_HOUR=${SUBTITLESKING_MCP_LIMIT_PER_HOUR}"
echo -e "    SUBTITLESKING_MCP_CONCURRENCY=${SUBTITLESKING_MCP_CONCURRENCY}"
echo -e "    SUBTITLESKING_MAX_UPLOAD_BYTES=${SUBTITLESKING_MAX_UPLOAD_BYTES}"
echo -e "    SUBTITLESKING_MAX_WORKERS=${SUBTITLESKING_MAX_WORKERS}"
echo -e "    SUBTITLESKING_WHISPER_MODEL=${SUBTITLESKING_WHISPER_MODEL}"
echo -e "    SUBTITLESKING_SRT_MAX_LINE_WIDTH=${SUBTITLESKING_SRT_MAX_LINE_WIDTH}"
echo -e "    SUBTITLESKING_SRT_MAX_LINE_COUNT=${SUBTITLESKING_SRT_MAX_LINE_COUNT}"
echo -e "    SUBTITLESKING_SRT_MAX_WORDS_PER_LINE=${SUBTITLESKING_SRT_MAX_WORDS_PER_LINE}"
