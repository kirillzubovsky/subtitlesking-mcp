#!/bin/bash -l
# Manual run script — useful for development and for environments where
# you'd rather "go run" from source than build a binary. Production
# deployments use deploy.sh instead, which compiles a binary and ships
# a systemd unit.
#
# Activates the Whisper Python venv at sk/ (relative to this script's
# directory) so child processes inherit `whisper` on PATH, then runs
# main.go which boots the upload server.
#
# Also preflight-checks that ffmpeg is installed and has the `subtitles`
# filter (libass) — the most common dev-environment foot-gun on macOS,
# where the default `brew install ffmpeg` formula no longer ships libass.

set -e

cd "$(dirname "$0")" || exit 1

# ── ffmpeg preflight ─────────────────────────────────────────────────────────
if ! command -v ffmpeg >/dev/null 2>&1; then
  cat >&2 <<'EOF'
✗ ffmpeg not found on PATH.

Install it for your platform:
  macOS:          brew tap homebrew-ffmpeg/ffmpeg && brew install homebrew-ffmpeg/ffmpeg/ffmpeg
  Ubuntu/Debian:  sudo apt-get install -y ffmpeg
  Fedora:         sudo dnf install -y ffmpeg
  Windows:        winget install Gyan.FFmpeg

The macOS line uses the libass-enabled tap on purpose — the default
`brew install ffmpeg` formula does not include libass and the burn-in
step will fail.
EOF
  exit 1
fi

if ! ffmpeg -hide_banner -filters 2>&1 | awk 'NF>=2 && $2=="subtitles" {found=1} END{exit !found}'; then
  cat >&2 <<'EOF'
✗ ffmpeg is installed but lacks the `subtitles` filter (libass missing).

Compression will work but the burn-in step will fail. On macOS, the
default `brew install ffmpeg` formula no longer ships libass. Install
the libass-enabled build:

    brew uninstall ffmpeg
    brew tap homebrew-ffmpeg/ffmpeg
    brew install homebrew-ffmpeg/ffmpeg/ffmpeg

Verify after install:
    ffmpeg -filters 2>&1 | grep -E '\bsubtitles\b'
EOF
  exit 1
fi

# ── Python venv ──────────────────────────────────────────────────────────────
if [ -f sk/bin/activate ]; then
  source sk/bin/activate
else
  echo "Warning: Python venv at sk/ not found — Whisper will be missing." >&2
  echo "Bootstrap it with:  python3 -m venv sk && sk/bin/pip install -r requirements.txt" >&2
fi

exec go run main.go
