# subtitlesking-mcp

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
and this binary expose identical behavior by construction. Open
source, MIT-licensed, **zero non-stdlib dependencies**, runs on
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
| `start_upload` | Reserve an upload slot for a `filename`. Returns `auth_token`, `upload_url`, and a copy-paste `curl` example. |
| `get_video_status` | Look up status by `auth_token`. Returns queue position, plus `transcript_url` once SRT is ready and `download_url` once the burned video is ready. |
| `get_transcript` | Return the SRT subtitle transcript inline. Available a couple of minutes before the burned video. |
| `get_download_url` | Return a 24-hour presigned URL for the finished, subtitle-burned video. The agent fetches the file out-of-band. |

Pipeline: **upload → ffmpeg compression → OpenAI Whisper transcription
→ ffmpeg subtitle burn-in**. Typical 3–10 min end-to-end; SRT is
usually ready 1–2 min earlier.

## Install

### Prebuilt binary (recommended)

Grab a binary from the
[releases page](https://github.com/kirillzubovsky/subtitlesking-mcp/releases),
unpack, and put it on your `PATH`. Builds are published for **macOS
(Apple Silicon and Intel)**, **Linux (amd64 and arm64)**, and
**Windows (amd64)**.

```bash
# macOS Apple Silicon
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-darwin-arm64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
subtitlesking-mcp --version
```

### From source

```bash
git clone https://github.com/kirillzubovsky/subtitlesking-mcp
cd subtitlesking-mcp
go build -o subtitlesking-mcp .
sudo mv subtitlesking-mcp /usr/local/bin/
```

Requires Go 1.22+. Zero non-stdlib dependencies.

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
1. start_upload({ filename: "clip.mp4" })
   → upload_url:   https://brains.subtitlesking.com/upload?presignedToken=…
     auth_token:   12345678
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

The full upload server (Whisper transcription, ffmpeg compression,
ffmpeg subtitle burn-in, REST API, web frontend) lives at
[github.com/kz-dev/subtitlesking](https://github.com/kz-dev/subtitlesking).
Run it locally, set `SUBTITLESKING_URL=http://localhost:8080`, and
this binary bridges your MCP client to it.

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

[mcp]: https://modelcontextprotocol.io
