#!/bin/bash -l
# Manual run script — useful for development and for environments where
# you'd rather "go run" from source than build a binary. Production
# deployments use deploy.sh instead, which compiles a binary and ships
# a systemd unit.
#
# Activates the Whisper Python venv at sk/ (relative to this script's
# directory) so child processes inherit `whisper` on PATH, then runs
# main.go which boots the upload server.

cd "$(dirname "$0")" || exit 1

if [ -f sk/bin/activate ]; then
  source sk/bin/activate
else
  echo "Warning: Python venv at sk/ not found — Whisper will be missing." >&2
  echo "Bootstrap it with:  python3 -m venv sk && sk/bin/pip install -r requirements.txt" >&2
fi

exec go run main.go
