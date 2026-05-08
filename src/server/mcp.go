// HTTP MCP endpoint for subtitlesking. Exposes the canonical tool surface
// for the subtitling pipeline. The downloadable stdio binary
// (cmd/mcp-server) is a thin proxy to this endpoint, so hosted and
// self-hosted MCP setups expose identical behavior.
//
// Upload contract: bytes never travel through the LLM. `start_upload`
// returns a presigned upload URL; the agent uploads bytes out-of-band
// (e.g. with curl). The same pattern applies to download via
// `get_download_url`.
//
// Transport: streamable-HTTP (POST one JSON-RPC request, get one response).
// Spec: https://modelcontextprotocol.io
package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/db"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	mcpServerName      = "subtitlesking"
	mcpServerVersion   = "2.0.0"
	mcpProtocolVersion = "2024-11-05"
)

// ── JSON-RPC types ────────────────────────────────────────────────────────────

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      any     `json:"id"`
	Result  any     `json:"result,omitempty"`
	Error   *mcpErr `json:"error,omitempty"`
}

type mcpErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpTextContent(text string) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// ── Tool definitions ──────────────────────────────────────────────────────────

var mcpTools = []map[string]any{
	{
		"name": "start_upload",
		"description": "Reserve an upload slot for a video to be subtitled. Returns an `upload_url` and a `curl_example` the caller runs themselves to upload the file out-of-band. Bytes never travel through the agent's context. Free tier supports videos up to 100 MB.\n\nProcessing time depends on the chosen `quality`:\n  • low    — Whisper `base` model. ~30 sec for a 2-min clip on CPU. Rough transcripts; OK for drafts, search indexing, content sketches.\n  • medium — Whisper `medium` model (default). ~3–6 min for a 2-min clip on CPU. Good general-purpose accuracy. Recommended for most uploads.\n  • high   — Whisper `large` model. ~7–15 min for a 2-min clip on CPU. Best accuracy, especially for proper nouns, accents, technical terms, and noisy audio.\n\nPick `low` when speed matters more than fidelity (e.g. internal review). Pick `high` for content that will be published or where transcript accuracy is load-bearing. `medium` is a sensible default.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename": map[string]any{
					"type":        "string",
					"description": "Original filename with extension, e.g. clip.mp4. The actual upload happens via the returned upload_url; the multipart 'file' field's filename is what the server records as authoritative.",
				},
				"quality": map[string]any{
					"type":        "string",
					"enum":        []string{"low", "medium", "high"},
					"description": "Transcription quality / speed trade-off. low = fast & rough (Whisper base), medium = default balanced (Whisper medium), high = accurate & slow (Whisper large). Defaults to medium when omitted.",
				},
			},
			"required": []string{"filename"},
		},
	},
	{
		"name":        "get_video_status",
		"description": "Check the processing status of a video previously submitted via start_upload. Statuses: pending_upload (slot reserved, bytes not yet received), new, compressing, compressed, generating_srt, srt_generated, burning_subtitles, subtitles_burned (done), error_*, deleted. Returns transcript_url once SRT is ready and download_url once the burned video is ready.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"auth_token": map[string]any{
					"type":        "string",
					"description": "The 8-digit auth_token returned from start_upload.",
				},
			},
			"required": []string{"auth_token"},
		},
	},
	{
		"name":        "get_transcript",
		"description": "Return the SRT subtitle transcript for a video as inline text. Available as soon as the pipeline reaches 'srt_generated' — typically a couple of minutes before the burned video is ready. Transcript and burned video are independent products: take either or both.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"auth_token": map[string]any{
					"type":        "string",
					"description": "The 8-digit auth_token from start_upload.",
				},
			},
			"required": []string{"auth_token"},
		},
	},
	{
		"name":        "get_download_url",
		"description": "Return a presigned URL for the finished, subtitle-burned video. Valid for 24 hours. The agent fetches the file out-of-band (e.g. with curl) — bytes never travel through agent context.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"auth_token": map[string]any{
					"type":        "string",
					"description": "The 8-digit auth_token from start_upload.",
				},
			},
			"required": []string{"auth_token"},
		},
	},
	// Deprecated tool stubs. Kept in tools/list for one release so existing
	// agents get a clear migration message instead of a silent disappearance.
	{
		"name":        "add_subtitles_to_video",
		"description": "DEPRECATED. Replaced by start_upload — uploading bytes through the LLM no longer works for non-trivial files. See https://www.subtitlesking.com/mcp.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		"name":        "download_subtitled_video",
		"description": "DEPRECATED. Replaced by get_download_url — agents now fetch bytes out-of-band via a presigned URL. See https://www.subtitlesking.com/mcp.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
}

// ── HTTP handler ──────────────────────────────────────────────────────────────

func handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, MCP-Protocol-Version, Mcp-Session-Id")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name":            mcpServerName,
			"version":         mcpServerVersion,
			"protocolVersion": mcpProtocolVersion,
			"transport":       "streamable-http",
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeMCPErr(w, nil, -32700, "could not read request body")
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMCPErr(w, nil, -32700, "parse error")
		return
	}

	switch req.Method {
	case "initialize":
		writeMCPResult(w, req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeMCPResult(w, req.ID, map[string]any{"tools": mcpTools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeMCPErr(w, req.ID, -32602, "invalid params")
			return
		}

		if !acquireMCPSlot() {
			writeMCPErr(w, req.ID, -32000, mcpFriendlyMessage("concurrency", 0))
			return
		}
		defer releaseMCPSlot()

		switch p.Name {
		case "start_upload":
			ip := clientIP(r)
			if ok, retryAfter := mcpUploadLimiter.allow(ip); !ok {
				log.Printf("handleMCP: rate-limited IP %s on start_upload (retry in %s)", ip, retryAfter)
				writeMCPErr(w, req.ID, -32000, mcpFriendlyMessage("rate_limit", retryAfter))
				return
			}
			mcpStartUpload(w, r, req.ID, p.Arguments)
		case "get_video_status":
			mcpGetVideoStatus(w, r, req.ID, p.Arguments)
		case "get_transcript":
			mcpGetTranscript(w, r, req.ID, p.Arguments)
		case "get_download_url":
			mcpGetDownloadURL(w, r, req.ID, p.Arguments)
		case "add_subtitles_to_video":
			writeMCPErr(w, req.ID, -32000,
				"add_subtitles_to_video has been replaced by start_upload. Bytes through the LLM no longer work for non-trivial files. See https://www.subtitlesking.com/mcp.")
		case "download_subtitled_video":
			writeMCPErr(w, req.ID, -32000,
				"download_subtitled_video has been replaced by get_download_url. Fetch bytes out-of-band via the returned URL. See https://www.subtitlesking.com/mcp.")
		default:
			writeMCPErr(w, req.ID, -32602, "unknown tool: "+p.Name)
		}
	default:
		writeMCPErr(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func writeMCPResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeMCPErr(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpErr{Code: code, Message: msg}})
}

// ── Tool implementations ─────────────────────────────────────────────────────

// qualityToWhisperModel maps the agent-facing quality token onto a
// concrete Whisper model name. Empty string → fall back to deploy
// default (handled in src/srt/main.go via SUBTITLESKING_WHISPER_MODEL).
// Unknown values are rejected at the caller, not silently coerced.
func qualityToWhisperModel(q string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "":
		return "", true // unset → deploy default
	case "low", "fast", "draft":
		return "base", true
	case "medium", "balanced", "default":
		return "medium", true
	case "high", "best", "accurate":
		return "large", true
	}
	return "", false
}

func mcpStartUpload(w http.ResponseWriter, r *http.Request, id any, rawArgs json.RawMessage) {
	var args struct {
		Filename string `json:"filename"`
		Quality  string `json:"quality"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		writeMCPErr(w, id, -32602, "invalid arguments")
		return
	}
	args.Filename = strings.TrimSpace(args.Filename)
	if args.Filename == "" {
		writeMCPErr(w, id, -32602, "filename is required")
		return
	}
	if strings.ContainsAny(args.Filename, "/\\") || args.Filename == "." || args.Filename == ".." {
		writeMCPErr(w, id, -32602, "filename must be a bare basename without path separators")
		return
	}

	whisperModel, ok := qualityToWhisperModel(args.Quality)
	if !ok {
		writeMCPErr(w, id, -32602, "quality must be one of: low, medium, high (or omit for default)")
		return
	}

	videoID := generateVideoID()
	authToken, err := db.AddVideoToDatabase(args.Filename, videoID, "", "pending_upload", whisperModel)
	if err != nil {
		log.Printf("mcpStartUpload: AddVideoToDatabase error: %v", err)
		writeMCPErr(w, id, -32603, "database error")
		return
	}

	_, uploadURL := issueUploadToken(r.Host, authToken)
	expiresAt := time.Now().Add(presignUploadTokenTTL).UTC().Format(time.RFC3339)

	curlExample := fmt.Sprintf("curl -F file=@/path/to/%s '%s'", args.Filename, uploadURL)

	// Surface the chosen quality back to the agent so they can confirm it
	// matches what the user asked for, and report typical processing time.
	qualityLabel := "medium (default)"
	if args.Quality != "" {
		qualityLabel = strings.ToLower(args.Quality)
	}
	timingHint := "3–6 min"
	if whisperModel == "base" {
		timingHint = "under 1 min"
	} else if whisperModel == "large" {
		timingHint = "7–15 min"
	}

	log.Printf("mcpStartUpload: reserved videoID=%s authToken=%s filename=%s quality=%s model=%q",
		videoID, authToken, args.Filename, qualityLabel, whisperModel)
	writeMCPResult(w, id, mcpTextContent(fmt.Sprintf(
		"Upload slot reserved. Run the following command to upload the video out-of-band — bytes do NOT need to flow through this conversation:\n\n%s\n\nvideo_id: %s\nauth_token: %s\nupload_url: %s\nexpires_at: %s\nmax_bytes: %d\nquality: %s\nexpected_processing_time: %s\n\nAfter the upload completes, poll get_video_status with auth_token to track progress.\n",
		curlExample, videoID, authToken, uploadURL, expiresAt, maxUploadBytes, qualityLabel, timingHint,
	)))
}

func mcpGetVideoStatus(w http.ResponseWriter, r *http.Request, id any, rawArgs json.RawMessage) {
	var args struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		writeMCPErr(w, id, -32602, "invalid arguments")
		return
	}
	if args.AuthToken == "" {
		writeMCPErr(w, id, -32602, "auth_token is required")
		return
	}

	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Printf("mcpGetVideoStatus: open db error: %v", err)
		writeMCPErr(w, id, -32603, "database error")
		return
	}
	defer dbConn.Close()

	rows, err := dbConn.Query("SELECT auth_token, status, video_id, created_at FROM videos WHERE auth_token = ?", args.AuthToken)
	if err != nil {
		log.Printf("mcpGetVideoStatus: query error: %v", err)
		writeMCPErr(w, id, -32603, "database query error")
		return
	}
	defer rows.Close()

	scheme := "https"
	if strings.HasPrefix(r.Host, "localhost") {
		scheme = "http"
	}

	var sb strings.Builder
	count := 0
	for rows.Next() {
		var authTokenValue, status, videoID, createdAt string
		if err := rows.Scan(&authTokenValue, &status, &videoID, &createdAt); err != nil {
			continue
		}
		count++
		if count > 1 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("Video %d:\n  status: %s\n", count, status))

		ahead := queuePositionAhead(dbConn, status, createdAt)
		if ahead > 0 {
			sb.WriteString(fmt.Sprintf("  queue_position: %d ahead\n", ahead))
		}

		token := generateDownloadTokenForVideo(videoID)
		if transcriptReady(status) {
			transcriptURL := fmt.Sprintf("%s://%s/transcript?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)
			sb.WriteString("  transcript_url: " + transcriptURL + "\n")
			sb.WriteString("  → Transcript (SRT) is ready. GET the URL or call get_transcript for inline text.\n")
		}

		switch {
		case status == "pending_upload":
			sb.WriteString("  → Slot reserved but bytes not yet received. Run the curl from start_upload.\n")
		case status == "subtitles_burned":
			downloadURL := fmt.Sprintf("%s://%s/download?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)
			sb.WriteString("  download_url: " + downloadURL + "\n")
			sb.WriteString("  → Subtitled video is ready. Call get_download_url, or fetch the URL above directly.\n")
		case strings.HasPrefix(status, "error_"):
			sb.WriteString("  → Error during processing.\n")
		default:
			sb.WriteString("  → Still processing. Check again in 30–60 seconds.\n")
		}
	}

	if count == 0 {
		writeMCPResult(w, id, mcpTextContent(fmt.Sprintf("No video found for auth_token %s.", args.AuthToken)))
		return
	}
	writeMCPResult(w, id, mcpTextContent(sb.String()))
}

func mcpGetTranscript(w http.ResponseWriter, r *http.Request, id any, rawArgs json.RawMessage) {
	var args struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		writeMCPErr(w, id, -32602, "invalid arguments")
		return
	}
	if args.AuthToken == "" {
		writeMCPErr(w, id, -32602, "auth_token is required")
		return
	}

	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		writeMCPErr(w, id, -32603, "database error")
		return
	}
	defer dbConn.Close()

	var videoID, status, file string
	err = dbConn.QueryRow("SELECT video_id, status, file FROM videos WHERE auth_token = ?", args.AuthToken).Scan(&videoID, &status, &file)
	if err == sql.ErrNoRows {
		writeMCPErr(w, id, -32602, "no video found for auth_token")
		return
	}
	if err != nil {
		writeMCPErr(w, id, -32603, "database query error")
		return
	}

	// The SRT file is the ground truth. Serve it whenever it's on disk —
	// even if the burn step subsequently errored (e.g. ffmpeg missing
	// libass), the transcript is still a complete, useful product. The
	// README explicitly promises that transcript and burned video are
	// independent; gating on status broke that promise.
	srtPath := transcriptPath(videoID, file)
	data, err := os.ReadFile(srtPath)
	if os.IsNotExist(err) {
		// File isn't on disk. Use the pipeline status to give a useful hint.
		switch {
		case status == "pending_upload":
			writeMCPErr(w, id, -32603, "transcript not yet available: bytes have not arrived (status=pending_upload)")
		case status == "new" || status == "compressing" || status == "compressed" || status == "generating_srt":
			writeMCPErr(w, id, -32603, "transcript not yet available: pipeline at "+status+", check back in 30–60s")
		case strings.HasPrefix(status, "error_"):
			writeMCPErr(w, id, -32603, "transcript not available: pipeline errored at "+status+" before SRT was generated")
		default:
			writeMCPErr(w, id, -32603, "transcript not yet available: status="+status)
		}
		return
	}
	if err != nil {
		log.Printf("mcpGetTranscript: read file error: %v", err)
		writeMCPErr(w, id, -32603, "could not read transcript: "+err.Error())
		return
	}

	writeMCPResult(w, id, mcpTextContent(fmt.Sprintf(
		"video_id: %s\nfilename: %s.srt\nformat: srt\n\n%s",
		videoID, strings.TrimSuffix(file, filepath.Ext(file)), string(data),
	)))
}

// presignDownloadTokenTTL mirrors the 24-hour expiry baked into
// generateDownloadTokenForVideo. Kept named so it can be reported back to
// agents alongside the URL.
const presignDownloadTokenTTL = 24 * time.Hour

func mcpGetDownloadURL(w http.ResponseWriter, r *http.Request, id any, rawArgs json.RawMessage) {
	var args struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		writeMCPErr(w, id, -32602, "invalid arguments")
		return
	}
	if args.AuthToken == "" {
		writeMCPErr(w, id, -32602, "auth_token is required")
		return
	}

	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		writeMCPErr(w, id, -32603, "database error")
		return
	}
	defer dbConn.Close()

	var videoID, status, file string
	err = dbConn.QueryRow("SELECT video_id, status, file FROM videos WHERE auth_token = ?", args.AuthToken).Scan(&videoID, &status, &file)
	if err == sql.ErrNoRows {
		writeMCPErr(w, id, -32602, "no video found for auth_token")
		return
	}
	if err != nil {
		writeMCPErr(w, id, -32603, "database query error")
		return
	}
	if status != "subtitles_burned" {
		writeMCPErr(w, id, -32603, "video is not ready: status="+status)
		return
	}

	scheme := "https"
	if strings.HasPrefix(r.Host, "localhost") {
		scheme = "http"
	}
	token := generateDownloadTokenForVideo(videoID)
	downloadURL := fmt.Sprintf("%s://%s/download?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)
	expiresAt := time.Now().Add(presignDownloadTokenTTL).UTC().Format(time.RFC3339)

	writeMCPResult(w, id, mcpTextContent(fmt.Sprintf(
		"Subtitled video ready. Fetch it out-of-band:\n\ncurl -o %s '%s'\n\nvideo_id: %s\nfilename: %s\ndownload_url: %s\nexpires_at: %s\n",
		file, downloadURL, videoID, file, downloadURL, expiresAt,
	)))
}
