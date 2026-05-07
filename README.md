# subtitlesking-mcp

**Companion repo for [SubtitlesKing.com](https://www.subtitlesking.com)** — the
AI video subtitle service this MCP plugs into. Try it in the browser at
[subtitlesking.com](https://www.subtitlesking.com), or use this MCP to drive
the same pipeline from Claude / Cursor / Windsurf.

> **MCP server for AI-generated video subtitles.** Give Claude Code,
> Claude Desktop, Cursor, Windsurf — any [Model Context Protocol][mcp]
> client — direct access to automatic subtitle generation. Powered by
> OpenAI Whisper transcription and ffmpeg burn-in via
> [Subtitles King](https://www.subtitlesking.com).

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

This binary is a thin **JSON-RPC 2.0 stdio↔HTTP bridge**: every request
read from stdin is POSTed verbatim to `<SUBTITLESKING_URL>/mcp`, and
the response is written back to stdout. All tool semantics live
server-side, so the hosted endpoint at
[brains.subtitlesking.com/mcp](https://brains.subtitlesking.com/mcp)
and this binary expose identical behavior by construction.
**Open-source**, MIT-licensed, **zero non-stdlib dependencies**, runs on
**macOS, Linux, and Windows**.

## Why use this

- **Subtitle videos through normal conversation.** "Claude, subtitle
  this clip" → Claude does the upload, polling, and download itself.
  No copy-paste, no separate dashboard, no five-tab workflow.
- **Real-size video uploads.** The MCP hands the agent a presigned URL
  instead of stuffing video bytes into the tool call. Agents can
  subtitle 100 MB videos through a model with an 8K context window.
- **Burn-in subtitles, baked SRT, or both.** The pipeline produces a
  hardcoded subtitle video *and* a standalone SRT transcript — take
  either or both, they're independent products.
- **Transcript ready first.** Whisper's SRT becomes available a
  couple of minutes before the burned video, so agents that only need
  the text don't have to wait for the ffmpeg burn-in step.
- **Self-host if you want.** Point `SUBTITLESKING_URL` at your own
  upload server (the full backend is at
  [github.com/kz-dev/subtitlesking](https://github.com/kz-dev/subtitlesking))
  for fully air-gapped, unlimited use.

## Tools

| Tool | What it does |
|---|---|
| `start_upload` | Reserve an upload slot for a `filename`. Optional `quality` arg (`low` / `medium` / `high`) selects the Whisper model. Returns `auth_token`, `upload_url`, and a copy-paste `curl` example. |
| `get_video_status` | Look up status by `auth_token`. Returns queue position, plus `transcript_url` once SRT is ready and `download_url` once the burned video is ready. |
| `get_transcript` | Return the SRT subtitle transcript inline. Available a couple of minutes before the burned video. |
| `get_download_url` | Return a 24-hour presigned URL for the finished, subtitle-burned video. The agent fetches the file out-of-band. |

### Quality / speed trade-off

The optional `quality` argument on `start_upload` lets the user pick a
specific accuracy/speed point per video. `medium` is the default and a
sensible balance for most uploads; agents can offer the user the choice
and pass through their answer.

| `quality` | Whisper model | Typical 2-min clip on CPU | When to use |
|---|---|---|---|
| `low` | `base` (~74M params) | under 1 min | Drafts, search indexing, content sketches. Rougher transcripts. |
| `medium` *(default)* | `medium` (~769M params) | 3–6 min | General-purpose. Good accuracy, reasonable speed. |
| `high` | `large` (~1.5B params) | 7–15 min | Published content, accents, technical jargon, noisy audio. Best accuracy. |

Omit `quality` to use the deploy-wide default (configurable on
self-hosted backends via `SUBTITLESKING_WHISPER_MODEL`).

Pipeline: **upload → ffmpeg compression → OpenAI Whisper transcription
→ ffmpeg subtitle burn-in**. End-to-end time depends on the chosen
quality (see table). The transcript is usually ready 1–2 min before the
burned video.

## Requirements

**You do NOT need to install Go, ffmpeg, Whisper, Python, or anything
else** to use this MCP server with the hosted backend. The prebuilt
binary is self-contained.

| What you want to do | What you need installed |
|---|---|
| Use the hosted MCP via URL (no binary at all) | Just an MCP client (Claude Code / Desktop / Cursor / Windsurf) |
| Use the prebuilt binary | The binary itself + `curl` (already on macOS, Linux, Windows 10+) |
| Build the binary from source | [Go 1.22+](https://go.dev/dl/) |
| **Self-host the full backend** (ffmpeg + Whisper) | See [github.com/kz-dev/subtitlesking](https://github.com/kz-dev/subtitlesking) — that's a separate repo |

The video transcoding tools (**ffmpeg**, **OpenAI Whisper**, Python)
run on the *server*, not your machine. When you use the hosted MCP,
those run on `brains.subtitlesking.com`. When you self-host, they run
on whichever server you set up.

## Install

### Option 1 — Use the hosted MCP (zero install)

If you just want to subtitle videos through Claude / Cursor /
Windsurf, you don't need to download this binary at all. Register the
hosted URL with your client and skip to the [Configure](#configure-your-mcp-client)
section:

```bash
claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp
```

Use this binary if your MCP client only speaks stdio (some older
versions of Claude Desktop, for example), or if you want to point at a
self-hosted backend.

### Option 2 — Prebuilt binary (recommended for stdio clients)

Builds are published for **macOS (Apple Silicon and Intel)**, **Linux
(amd64 and arm64)**, and **Windows (amd64)** on the
[releases page](https://github.com/kirillzubovsky/subtitlesking-mcp/releases).

#### macOS (Apple Silicon)

```bash
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-darwin-arm64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
subtitlesking-mcp --version
```

#### macOS (Intel)

```bash
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-darwin-amd64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
```

#### Linux (amd64)

```bash
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-linux-amd64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
```

#### Linux (arm64, e.g. Raspberry Pi 4 / 5, Apple Silicon Linux VMs)

```bash
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-linux-arm64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
```

#### Windows (PowerShell)

```powershell
Invoke-WebRequest -Uri https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-windows-amd64.zip -OutFile subtitlesking-mcp.zip
Expand-Archive subtitlesking-mcp.zip
# Move subtitlesking-mcp.exe somewhere on your PATH
```

> **macOS Gatekeeper warning?** If macOS refuses to launch the
> downloaded binary the first time, run
> `xattr -d com.apple.quarantine /usr/local/bin/subtitlesking-mcp`
> once and try again.

### Option 3 — Build from source

You need [Go 1.22 or newer](https://go.dev/dl/). If you don't have Go
yet:

```bash
# macOS (Homebrew)
brew install go

# Ubuntu / Debian
sudo apt-get update && sudo apt-get install -y golang-go

# Fedora
sudo dnf install -y golang

# Arch
sudo pacman -S go

# Windows: download installer from https://go.dev/dl/
```

Then:

```bash
git clone https://github.com/kirillzubovsky/subtitlesking-mcp
cd subtitlesking-mcp
go build -o subtitlesking-mcp .
sudo mv subtitlesking-mcp /usr/local/bin/
subtitlesking-mcp --version
```

Zero non-stdlib dependencies — no `go.sum`, no `vendor/` tree, no
package downloads. The build is offline-capable once you have Go.

## Configure your MCP client

### Claude Code

```bash
# Hosted MCP — no install needed
claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp

# Or use this binary
claude mcp add subtitlesking /usr/local/bin/subtitlesking-mcp
```

### Claude Desktop / Cursor / Windsurf / Zed / others

```json
{
  "mcpServers": {
    "subtitlesking": {
      "command": "/usr/local/bin/subtitlesking-mcp"
    }
  }
}
```

### Self-hosted backend

```json
{
  "mcpServers": {
    "subtitlesking": {
      "command": "/usr/local/bin/subtitlesking-mcp",
      "env": {
        "SUBTITLESKING_URL": "http://localhost:8080"
      }
    }
  }
}
```

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `SUBTITLESKING_URL` | `https://brains.subtitlesking.com` | Backend `/mcp` endpoint to forward to. |

## Example agent flow

```
1. start_upload({ filename: "clip.mp4", quality: "medium" })
   → upload_url:   https://brains.subtitlesking.com/upload?presignedToken=…
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

`error_*` is emitted if any step fails.

## FAQ

**I'm new to all of this. What do I actually do?**
Easiest path: install [Claude Code](https://docs.anthropic.com/claude-code)
or [Claude Desktop](https://claude.ai/download), then run
`claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp`
(or paste the JSON snippet from
[Configure](#configure-your-mcp-client) into your client's MCP config
file). You don't need to download this binary, install Go, or install
ffmpeg. Restart your client, and Claude can subtitle videos for you
in normal conversation.

**Do I need to know Go?**
No. Use the prebuilt binary (or just the hosted URL). You only need
Go if you want to build the binary from source yourself.

**What is the Model Context Protocol (MCP)?**
MCP is an open protocol from Anthropic that standardizes how AI
agents talk to external tools. Any MCP-compatible client — Claude
Code, Claude Desktop, Cursor, Windsurf, Zed, Continue, and others —
can use this server without writing integration code.

**Do I need an API key?**
No. The bridge forwards every request to `SUBTITLESKING_URL/mcp`,
which mints presigned URLs internally. Free-tier limits apply on the
hosted backend; self-host for unlimited use.

**Can I use this with a long video?**
The free tier caps uploads at 100 MB. Self-host for any size — the
upload happens out-of-band, so file size never depends on the LLM's
context window.

**Why a separate binary if there's a hosted MCP?**
Some MCP clients don't yet speak streamable-HTTP, and stdio is the
universal transport. The binary also lets you point at a self-hosted
upload server with one env var.

**What models drive the pipeline?**
OpenAI Whisper (large model) for speech-to-text, ffmpeg for video
compression and subtitle burn-in.

## Self-hosting the upload server

If you want to run the entire pipeline on your own hardware — for
privacy, regulated workloads, or just unlimited use — clone the
backend repo:
[github.com/kz-dev/subtitlesking](https://github.com/kz-dev/subtitlesking).

The backend is what actually does the work, and **it has real system
dependencies** (whereas this bridge has none):

- **ffmpeg** for video compression and subtitle burn-in.
  - macOS: `brew install ffmpeg`
  - Ubuntu / Debian: `sudo apt-get install ffmpeg`
  - Fedora: `sudo dnf install ffmpeg`
  - Windows: `winget install Gyan.FFmpeg` or
    [download from ffmpeg.org](https://ffmpeg.org/download.html)
- **OpenAI Whisper** for speech-to-text. Install with pip
  (`pip install -U openai-whisper`) or
  [from source](https://github.com/openai/whisper). Whisper itself
  needs Python 3.8+ and PyTorch.
- **Go 1.22+** to build and run the upload server.
- **A few GB of disk** for Whisper model weights. The default is the
  `medium` model (~1.5 GB); switching to `large` adds ~1.5 GB more.
- **A reasonably beefy CPU or GPU.** Whisper is the slow step.

The backend exposes a `SUBTITLESKING_WHISPER_MODEL` env var that picks
the default model when an upload doesn't specify a `quality` arg. Set
it on the systemd unit (or your container env) to `base`, `small`,
`medium`, or `large`. Per-upload overrides via the MCP `quality` arg
always take precedence over this default.

Once the backend is running locally, point this MCP bridge at it:

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

## Related

- [Subtitles King](https://www.subtitlesking.com) — the product.
- [Subtitles King MCP docs](https://www.subtitlesking.com/docs/mcp) — full reference.
- [Self-host guide](https://www.subtitlesking.com/docs/self-host).
- [Hosted vs self-host comparison](https://www.subtitlesking.com/vs/hosted-vs-self-hosted-mcp).

## Protocol

JSON-RPC 2.0. The stdio binary uses newline-delimited JSON-RPC over
stdin/stdout. The hosted MCP uses streamable-HTTP: POST one request,
receive one response. Spec:
[modelcontextprotocol.io][mcp].

## License

[MIT](./LICENSE).

## Author

Built and maintained by [Kirill Zubovsky](https://kirillzubovsky.com).
Other projects, posts, and how to reach me are at
[kirillzubovsky.com](https://kirillzubovsky.com). Pull requests and
issues welcome.

[mcp]: https://modelcontextprotocol.io
