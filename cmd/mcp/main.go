// subtitlesking-mcp is a Model Context Protocol (MCP) bridge for the
// Subtitles King video-subtitling backend. It is a thin JSON-RPC
// stdio↔HTTP proxy: every line read from stdin is POSTed verbatim to
// <SUBTITLESKING_URL>/mcp, and the response is written back to stdout.
//
// All tool semantics live server-side, so the hosted endpoint
// (https://brains.subtitlesking.com/mcp) and this binary expose
// identical behavior by construction. Bytes never travel through the
// agent's context — start_upload returns a presigned URL the agent uses
// out-of-band (e.g. with curl).
//
// Set SUBTITLESKING_URL to point at a self-hosted upload server, e.g.
// http://localhost:8080.
//
// Protocol: JSON-RPC 2.0 over stdio (newline-delimited).
// Spec: https://modelcontextprotocol.io
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// version is overwritten at build time via ldflags. Defaults to "dev"
// for local builds (`go build`); release binaries get the tag.
var version = "dev"

// baseURL is the upstream MCP endpoint to forward to. Override with
// SUBTITLESKING_URL to point at a self-hosted upload server.
func baseURL() string {
	if u := os.Getenv("SUBTITLESKING_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://brains.subtitlesking.com"
}

// emitErr writes a JSON-RPC error response to stdout. Used when the
// bridge itself fails (network/transport) before the upstream can
// answer.
func emitErr(id any, code int, message string) {
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
	b, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", b)
}

func main() {
	// `subtitlesking-mcp --version` for support / diagnostics.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("subtitlesking-mcp %s (upstream: %s)\n", version, baseURL())
		return
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}

	scanner := bufio.NewScanner(os.Stdin)
	// 1 MB per JSON-RPC line — videos do not flow through this bridge,
	// so the heaviest realistic payload is an inline SRT transcript.
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		// Pull the id (best-effort) so a transport-layer failure can be
		// reported back as a JSON-RPC error correlating to the request.
		var probe struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &probe)

		// Notifications carry no id and expect no response.
		isNotification := probe.ID == nil && strings.HasPrefix(probe.Method, "notifications/")

		req, err := http.NewRequest("POST", baseURL()+"/mcp", bytes.NewReader(line))
		if err != nil {
			if !isNotification {
				emitErr(probe.ID, -32603, "bridge: build request: "+err.Error())
			}
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "subtitlesking-mcp/"+version)

		resp, err := httpClient.Do(req)
		if err != nil {
			if !isNotification {
				emitErr(probe.ID, -32603, "bridge: upstream unreachable: "+err.Error())
			}
			continue
		}

		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			if !isNotification {
				emitErr(probe.ID, -32603, "bridge: read upstream: "+rerr.Error())
			}
			continue
		}

		if isNotification {
			// Upstream returns 202 with empty body for notifications/initialized.
			continue
		}

		if len(bytes.TrimSpace(body)) == 0 {
			emitErr(probe.ID, -32603, fmt.Sprintf("bridge: upstream returned empty body (HTTP %d)", resp.StatusCode))
			continue
		}

		// Pass through verbatim — upstream already speaks JSON-RPC.
		fmt.Fprintf(os.Stdout, "%s\n", bytes.TrimRight(body, "\n"))
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "subtitlesking-mcp: stdin scan error: %v\n", err)
		os.Exit(1)
	}
}
