# subtitlesking-mcp

[![Release](https://img.shields.io/github/v/release/kirillzubovsky/subtitlesking-mcp?style=flat-square)](https://github.com/kirillzubovsky/subtitlesking-mcp/releases)
[![License](https://img.shields.io/github/license/kirillzubovsky/subtitlesking-mcp?style=flat-square)](./LICENSE)

A [Model Context Protocol](https://modelcontextprotocol.io) server that lets
any MCP-compatible AI agent — Claude Code, Claude Desktop, Cursor, Windsurf,
others — add AI-generated subtitles to videos via
[Subtitles King](https://www.subtitlesking.com).

**Bytes never travel through the agent's context.** The MCP returns a
presigned upload URL and the agent uploads the video out-of-band with
`curl`. Same for download. This means the upload tool actually works at
real video sizes — not just whatever fits in the LLM's context window.

This binary is a thin **JSON-RPC stdio↔HTTP bridge**: every request from
stdin is POSTed verbatim to `<SUBTITLESKING_URL>/mcp`, and the response is
written back to stdout. All tool semantics live server-side. The hosted
endpoint at `https://brains.subtitlesking.com/mcp` and this binary expose
identical behavior by construction.

## Tools

| Tool | Description |
|---|---|
| `start_upload` | Reserve an upload slot for a `filename`. Returns `auth_token`, `upload_url`, and a copy-paste `curl` example. |
| `get_video_status` | Look up status by `auth_token`. Returns queue position, plus `transcript_url` once SRT is ready and `download_url` once the burned video is ready. |
| `get_transcript` | Return the SRT transcript inline (it's small). Available a couple of minutes before the burned video. |
| `get_download_url` | Return a 24-hour presigned URL for the finished, subtitle-burned video. The agent fetches the file out-of-band. |

Pipeline: upload → ffmpeg compression → Whisper transcription → ffmpeg
subtitle burn-in. Typical 3–10 min end-to-end; transcript is usually ready
1–2 min earlier.

## Install

### Prebuilt binary (recommended)

Grab a binary from the
[releases page](https://github.com/kirillzubovsky/subtitlesking-mcp/releases),
unpack, and put it on your `PATH`:

```bash
# macOS arm64 example
curl -L https://github.com/kirillzubovsky/subtitlesking-mcp/releases/latest/download/subtitlesking-mcp-darwin-arm64.tar.gz \
  | tar -xz
sudo mv subtitlesking-mcp /usr/local/bin/
```

### From source

```bash
git clone https://github.com/kirillzubovsky/subtitlesking-mcp
cd subtitlesking-mcp
go build -o subtitlesking-mcp .
sudo mv subtitlesking-mcp /usr/local/bin/
```

The binary has zero non-stdlib dependencies.

## Configure your client

### Claude Code

```bash
# Hosted MCP — no install needed
claude mcp add --transport http subtitlesking https://brains.subtitlesking.com/mcp

# Or use this binary
claude mcp add subtitlesking /usr/local/bin/subtitlesking-mcp
```

### Claude Desktop / Cursor / Windsurf

```json
{
  "mcpServers": {
    "subtitlesking": {
      "command": "/usr/local/bin/subtitlesking-mcp"
    }
  }
}
```

### Pointing at a self-hosted upload server

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

2. (the agent runs the curl itself; bytes go disk→server)

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

You can take only the transcript, only the burned video, or both — they're
independent products.

## Self-hosting the upload server

The full upload server (Whisper transcription, ffmpeg compression, ffmpeg
subtitle burn-in) lives at
[github.com/kz-dev/subtitlesking](https://github.com/kz-dev/subtitlesking).
Run it locally and set `SUBTITLESKING_URL=http://localhost:8080` to bridge
this binary to your own backend.

## Protocol

JSON-RPC 2.0 over stdio (newline-delimited messages). Spec:
[modelcontextprotocol.io](https://modelcontextprotocol.io).

## License

[MIT](./LICENSE).
