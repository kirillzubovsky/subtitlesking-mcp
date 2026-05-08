# subtitlesking-mcp

**Companion repo for [SubtitlesKing.com](https://www.subtitlesking.com)** — the
AI video subtitle service this MCP plugs into. Try it in the browser at
[subtitlesking.com](https://www.subtitlesking.com), or use this MCP to drive
the same pipeline from Claude / Cursor / Windsurf.

> **MCP server for AI-generated video subtitles.** Give Claude Code,
> Claude Desktop, Cursor, Windsurf — any [Model Context Protocol][mcp]
> client — direct access to automatic subtitle generation. Powered by
> OpenAI Whisper transcription and ffmpeg burn-in.

[![Release](https://img.shields.io/github/v/release/kirillzubovsky/subtitlesking-mcp?style=flat-square)](https://github.com/kirillzubovsky/subtitlesking-mcp/releases)
[![Downloads](https://img.shields.io/github/downloads/kirillzubovsky/subtitlesking-mcp/total?style=flat-square)](https://github.com/kirillzubovsky/subtitlesking-mcp/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/kirillzubovsky/subtitlesking-mcp?style=flat-square)](./go.mod)
[![License](https://img.shields.io/github/license/kirillzubovsky/subtitlesking-mcp?style=flat-square)](./LICENSE)
[![MCP](https://img.shields.io/badge/MCP-compatible-blue?style=flat-square)](https://modelcontextprotocol.io)

**Bytes never travel through the AI agent's context.** Most MCP video
tools fail on real files because they try to base64-encode the video
into a tool-call payload — a 100 MB clip becomes 130+ MB of base64 that
must fit inside the model's context window. This server avoids that
entirely: `start_upload` returns a presigned upload URL and the agent
uploads the file out-of-band with `curl`. Same on the way out via
`get_download_url`. The upload tool just *works*, regardless of file
size or model context.

This repo is **everything you need to run the full Subtitles King MCP
on your own machine** — both the lightweight stdio↔HTTP MCP bridge
*and* the upload server that actually runs the
ffmpeg → Whisper → ffmpeg-burn-in pipeline. Open-source, MIT-licensed,
runs on **macOS, Linux, and Windows**.

## What's in this repo

| Path | What it is |
|---|---|
| `cmd/mcp/main.go` | **MCP bridge** — thin JSON-RPC stdio↔HTTP proxy. Forwards every request from your MCP client to a Subtitles King upload server. The thing your Claude Code / Cursor / Windsurf launches. Zero non-stdlib deps. |
| `main.go` + `src/` | **Upload server** — the actual subtitling pipeline. HTTP API, MCP HTTP endpoint, SQLite job queue, ffmpeg compression, OpenAI Whisper transcription, ffmpeg subtitle burn-in. This is what the bridge talks to. |
| `deploy.sh`, `status.sh` | Operations scripts for self-hosters. Idempotent deploy with auto-heal (creates Whisper venv, installs system packages, builds binary, writes systemd unit). Health-check + rollback support. |
| `requirements.txt` | Python deps for the Whisper venv (torch, openai-whisper, etc.) |
| `Dockerfile` | Container image of the full upload server. |

## How you use it (three modes)

### Mode 1 — Hosted, zero install

The lowest-friction path. Skip cloning this repo entirely. Register the
hosted endpoint with your MCP client and you're done:

```bash
claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp
```

Or in `claude_desktop_config.json` / `mcp.json`:

```json
{
  "mcpServers": {
    "subtitlesking": {
      "url": "https://brains.subtitlesking.com/mcp"
    }
  }
}
```

Free-tier limits apply (videos up to 100 MB, 24 h retention).

### Mode 2 — Run the stdio bridge locally, talk to the hosted backend

Use this when your MCP client only speaks stdio (some older Claude
Desktop / IDE integrations), or you'd rather not register an HTTP URL.

Grab a prebuilt bridge binary from the
[releases page](https://github.com/kirillzubovsky/subtitlesking-mcp/releases)
or build it yourself:

```bash
git clone https://github.com/kirillzubovsky/subtitlesking-mcp
cd subtitlesking-mcp
go build -o subtitlesking-mcp ./cmd/mcp
sudo mv subtitlesking-mcp /usr/local/bin/
```

Register with Claude Code:

```bash
claude mcp add subtitlesking /usr/local/bin/subtitlesking-mcp
```

Same Whisper pipeline, same free-tier limits as Mode 1 — the bridge
just forwards every JSON-RPC call to `https://brains.subtitlesking.com/mcp`.

### Mode 3 — Full self-host

Run the entire pipeline (Whisper transcription, ffmpeg compression and
burn-in, REST/MCP API) on your own hardware. Good for privacy,
regulated workloads, larger files, unlimited usage.

Two steps:

**1. Run the upload server.** It needs `ffmpeg`, Python 3 with
`openai-whisper` installed, and Go 1.22+ to build. The fastest way is
`./deploy.sh` against a fresh Debian/Ubuntu VPS — it auto-heals system
packages, bootstraps the Whisper venv, builds the binary, writes a
systemd unit, and starts the service. See
[Self-hosting](#self-hosting-the-upload-server) below.

For local dev you can also just:

```bash
# macOS — use the libass-enabled tap (default brew formula doesn't include
# libass, which the subtitle burn-in step requires)
brew tap homebrew-ffmpeg/ffmpeg
brew install homebrew-ffmpeg/ffmpeg/ffmpeg

# or, on Ubuntu / Debian:
sudo apt-get install -y ffmpeg

python3 -m venv sk
sk/bin/pip install -r requirements.txt
./start.sh                                  # boots the server on :8080
```

`start.sh` runs a preflight check that ffmpeg has the `subtitles`
filter and refuses to boot if libass is missing — fail loudly rather
than silently producing burn errors mid-pipeline.

**2. Point the bridge at it.** Same binary as Mode 2, just with one
env var:

```json
{
  "mcpServers": {
    "subtitlesking": {
      "command": "/usr/local/bin/subtitlesking-mcp",
      "env": { "SUBTITLESKING_URL": "http://localhost:8080" }
    }
  }
}
```

## Tools

| Tool | What it does |
|---|---|
| `start_upload` | Reserve an upload slot for a `filename`. Optional `quality` arg (`low` / `medium` / `high`) selects the Whisper model. Returns `auth_token`, `upload_url`, and a copy-paste `curl` example. |
| `get_video_status` | Look up status by `auth_token`. Returns queue position, plus `transcript_url` once SRT is ready and `download_url` once the burned video is ready. |
| `get_transcript` | Return the SRT subtitle transcript inline. Available a couple of minutes before the burned video. |
| `get_download_url` | Return a 24-hour presigned URL for the finished, subtitle-burned video. The agent fetches the file out-of-band. |

### Quality / speed trade-off

The optional `quality` argument on `start_upload` picks which Whisper
model transcribes the audio. `medium` is the default — sensible balance
of speed and accuracy on CPU. Agents can ask the user and pass through
the answer.

| `quality` | Whisper model | Typical 2-min clip on CPU | When to use |
|---|---|---|---|
| `low` | `base` (~74M params) | under 1 min | Drafts, search indexing, content sketches. Rougher transcripts. |
| `medium` *(default)* | `medium` (~769M params) | 3–6 min | General-purpose. Good accuracy, reasonable speed. |
| `high` | `large` (~1.5B params) | 7–15 min | Published content, accents, technical jargon, noisy audio. Best accuracy. |

Omit `quality` to use the deploy-wide default (configurable on
self-hosted backends via `SUBTITLESKING_WHISPER_MODEL`).

### Subtitle chunking

By default, burned subtitles are wrapped to **42 chars × max 2 lines**
per chunk — broadcast-safe convention that yields ~3–5 second segments
at conversational speech rates. Tunable on self-hosted backends:

| Env var | Default | What it does |
|---|---|---|
| `SUBTITLESKING_SRT_MAX_LINE_WIDTH` | `42` | Chars per line. |
| `SUBTITLESKING_SRT_MAX_LINE_COUNT` | `2` | Lines per chunk. |
| `SUBTITLESKING_SRT_MAX_WORDS_PER_LINE` | (unset) | Set to e.g. `4` for TikTok-style short bursts. Mutually exclusive with the width/count pair. |

## Example agent flow

```
1. start_upload({ filename: "clip.mp4", quality: "medium" })
   → upload_url:   https://.../upload?presignedToken=…
     auth_token:   12345678
     quality:      medium
     expected_processing_time: 3–6 min
     curl_example: curl -F file=@/path/to/clip.mp4 '<upload_url>'

2. (the agent runs the curl itself; bytes go disk-to-server)

3. get_video_status({ auth_token: "12345678" })
   → status: srt_generated
     transcript_url: https://…

4. get_transcript({ auth_token: "12345678" })
   → (inline SRT text)

5. get_video_status({ auth_token: "12345678" })
   → status: subtitles_burned

6. get_download_url({ auth_token: "12345678" })
   → curl -o clip-subtitled.mp4 'https://…'

7. (the agent runs the curl itself)
```

## Status states

```
pending_upload    → slot reserved, bytes not yet received
new               → bytes received, queued for processing
compressing       → compressed
generating_srt    → srt_generated   (transcript ready)
burning_subtitles → subtitles_burned   (video ready)
```

`error_*` is emitted if any step fails. Rows stuck in `pending_upload`
are pruned automatically after 1 hour.

## Requirements

| What you want to do | What you need installed |
|---|---|
| Use the hosted MCP via URL (Mode 1) | Just an MCP client (Claude Code / Desktop / Cursor / Windsurf) |
| Use the prebuilt bridge binary (Mode 2) | Just the binary + `curl` (already on macOS, Linux, Windows 10+) |
| Build the bridge from source | [Go 1.22+](https://go.dev/dl/) |
| **Self-host the full backend** (Mode 3) | ffmpeg, Python 3.8+, Go 1.22+, ~5 GB disk for Whisper weights |

## Self-hosting the upload server

The upload server has real system dependencies — this is what does the
work, not the bridge.

### One-shot deploy to a fresh VPS

`deploy.sh` is idempotent and auto-heals: it installs missing apt
packages (`ffmpeg`, `python3-venv`, `sqlite3`, `lsof`), bootstraps the
Whisper venv at `sk/`, builds the upload-server binary on the box,
writes a systemd unit with all the right env vars, and starts the
service. First run takes ~5 min and ~5 GB of disk (torch + Whisper
weights). Subsequent runs are seconds.

```bash
# Edit deploy.sh and set SERVER_HOST + DOMAIN to your own values, OR
# pass them inline:
SERVER_HOST=1.2.3.4 DOMAIN=mcp.example.com ./deploy.sh
```

The script preserves `videos.db`, `files/`, `sk/`, and `letsencrypt/`
across deploys — you won't lose in-flight jobs or your venv.

### Run from source locally (no systemd)

```bash
# 1. Install ffmpeg WITH libass — required for the subtitle burn-in
brew tap homebrew-ffmpeg/ffmpeg                                # macOS
brew install homebrew-ffmpeg/ffmpeg/ffmpeg                     # macOS

sudo apt-get install -y ffmpeg                                 # Ubuntu / Debian
sudo dnf install -y ffmpeg                                     # Fedora
winget install Gyan.FFmpeg                                     # Windows

# Verify libass is present (the line should match "subtitles"):
ffmpeg -filters 2>&1 | grep -E '\bsubtitles\b'

# 2. Bootstrap the Whisper venv (~5 min, ~5 GB)
python3 -m venv sk
sk/bin/pip install --upgrade pip
sk/bin/pip install -r requirements.txt

# 3. Run the upload server
./start.sh                          # boots on :8080
```

### Operations

```bash
./status.sh                         # quick status overview
./status.sh doctor                  # deep deployment health check
./status.sh logs                    # tail the journal
./status.sh queue                   # current pipeline state
./status.sh rollback                # restore previous deploy from backup
```

### Configuration

The systemd unit baked by `deploy.sh` honors these env vars:

| Env var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP port for the upload server. |
| `SUBTITLESKING_WHISPER_MODEL` | `medium` | Whisper model when an upload doesn't supply a `quality` arg. |
| `SUBTITLESKING_SRT_MAX_LINE_WIDTH` | `42` | Subtitle wrapping. |
| `SUBTITLESKING_SRT_MAX_LINE_COUNT` | `2` | Max lines per subtitle chunk. |
| `SUBTITLESKING_SRT_MAX_WORDS_PER_LINE` | — | TikTok-style chunking; overrides width/count. |
| `SUBTITLESKING_MAX_UPLOAD_BYTES` | `104857600` | 100 MB upload cap. |
| `SUBTITLESKING_MAX_WORKERS` | `2` | Concurrent video processors. |
| `SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR` | `5` | Per-IP rate limit on `/presign`. |
| `SUBTITLESKING_MCP_LIMIT_PER_HOUR` | `5` | Per-IP rate limit on `start_upload`. |
| `SUBTITLESKING_MCP_CONCURRENCY` | `10` | Server-wide cap on concurrent MCP calls. |

Override at deploy time: `SUBTITLESKING_WHISPER_MODEL=large ./deploy.sh`.

## Configure your MCP client (any mode)

### Claude Code

```bash
claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp
# or, for stdio:
claude mcp add subtitlesking /usr/local/bin/subtitlesking-mcp
```

### Claude Desktop / Cursor / Windsurf / Zed

```json
{
  "mcpServers": {
    "subtitlesking": {
      "command": "/usr/local/bin/subtitlesking-mcp"
    }
  }
}
```

## FAQ

**I'm new to all of this. What do I actually do?**
Easiest: install [Claude Code](https://docs.anthropic.com/claude-code) or
[Claude Desktop](https://claude.ai/download), then run
`claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp`.
Restart your client. Claude can now subtitle videos for you in normal
conversation. You don't need to download anything from this repo.

**Do I need to know Go?**
No. Use the prebuilt binary or just the hosted URL. Go is only needed
if you build from source.

**What is the Model Context Protocol (MCP)?**
MCP is an open protocol from Anthropic that standardizes how AI agents
talk to external tools. Any MCP-compatible client — Claude Code,
Claude Desktop, Cursor, Windsurf, Zed, Continue, and others — can use
this server without writing integration code.

**Do I need an API key?**
No, not on the bridge or the hosted endpoint. The upload server has a
hardcoded placeholder API key (`MY_AWESOME_API_KEY` in
`src/server/main.go`) for direct REST API use; change it before
exposing publicly.

**Can I use this with a long video?**
The hosted free tier caps uploads at 100 MB. Self-host (Mode 3) for
any size — file size never depends on the LLM's context window because
the upload happens out-of-band.

**Why a bridge if there's a hosted MCP?**
Some MCP clients don't yet speak streamable-HTTP, and stdio is the
universal transport. The bridge also lets you point at a self-hosted
upload server with one env var.

**What models drive the pipeline?**
OpenAI Whisper for speech-to-text (model selectable via the `quality`
arg or `SUBTITLESKING_WHISPER_MODEL`), ffmpeg for video compression
and subtitle burn-in.

**Burn step keeps failing on macOS — what's wrong?**
Almost always the ffmpeg build. The default `brew install ffmpeg` on
modern Homebrew doesn't include libass, which the subtitle burn-in
requires. Verify with:

```bash
ffmpeg -filters 2>&1 | grep -E '\bsubtitles\b'
```

If that prints nothing, install the libass-enabled tap:

```bash
brew uninstall ffmpeg
brew tap homebrew-ffmpeg/ffmpeg
brew install homebrew-ffmpeg/ffmpeg/ffmpeg
```

The transcript (SRT) is still produced when burn fails — `get_transcript`
serves it regardless of pipeline status, since transcript and burned
video are independent products.

## Related

- [Subtitles King](https://www.subtitlesking.com) — the hosted product.
- [MCP docs](https://www.subtitlesking.com/docs/mcp) — extended reference.
- [Self-host guide](https://www.subtitlesking.com/docs/self-host).

## Protocol

JSON-RPC 2.0. The stdio bridge uses newline-delimited JSON-RPC over
stdin/stdout. The HTTP MCP endpoint uses streamable-HTTP: POST one
request, receive one response. Spec:
[modelcontextprotocol.io][mcp].

## License

[MIT](./LICENSE).

## Author

Built and maintained by [Kirill Zubovsky](https://kirillzubovsky.com).
Other projects, posts, and how to reach me are at
[kirillzubovsky.com](https://kirillzubovsky.com). Pull requests and
issues welcome.

[mcp]: https://modelcontextprotocol.io
