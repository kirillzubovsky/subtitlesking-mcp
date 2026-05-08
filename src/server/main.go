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
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const apiKey = "MY_AWESOME_API_KEY"

var (
	presignedTokens sync.Map // key: token string, value: presignedTokenEntry
	downloadTokens  sync.Map // key: token string, value: downloadTokenEntry
)

// presignedTokenEntry binds an upload token to the row it should fill.
// authToken is empty for the webform path (legacy: /upload generates a new
// videoID and DB row). When non-empty, /upload writes into the existing row
// the MCP `start_upload` tool created and flips its status from
// "pending_upload" to "new".
type presignedTokenEntry struct {
	authToken string
	expiry    time.Time
}

type downloadTokenEntry struct {
	videoID string
	expiry  time.Time
}

func generateDownloadTokenForVideo(videoID string) string {
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	entry := downloadTokenEntry{
		videoID: videoID,
		expiry:  time.Now().Add(24 * time.Hour),
	}
	downloadTokens.Store(token, entry)
	return token
}

func StartUploadServer() {
	http.HandleFunc("/upload", cors(handleUpload))
	http.HandleFunc("/queue", handleQueue)
	http.HandleFunc("/presign", cors(handlePresign))
	http.HandleFunc("/downloadpresign", cors(handleDownloadPresign))
	http.HandleFunc("/download", cors(handleDownload))
	http.HandleFunc("/transcript", cors(handleTranscript))
	http.HandleFunc("/mcp", handleMCP)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("StartUploadServer: Listening on :%s for file uploads, queue, presign and download requests...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))

	// NGINX handles SSL and directs requests to this server.
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("cors: processing %s request for %s", r.Method, r.URL.String())
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-KEY")

		if r.Method == http.MethodOptions {
			log.Printf("cors: OPTIONS request received, returning OK")
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

// presignUploadTokenTTL is how long a presigned upload URL stays valid.
// Matches the worker's pending_upload cleanup window so a row that was
// claimed but never uploaded can't outlive its claim.
const presignUploadTokenTTL = time.Hour

// issueUploadToken mints a presigned upload token bound to the given
// authToken (may be empty for the webform path). Returns the token and the
// fully-qualified upload URL.
func issueUploadToken(host, authToken string) (string, string) {
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	presignedTokens.Store(token, presignedTokenEntry{
		authToken: authToken,
		expiry:    time.Now().Add(presignUploadTokenTTL),
	})

	scheme := "https"
	if strings.HasPrefix(host, "localhost") {
		scheme = "http"
	}
	uploadURL := fmt.Sprintf("%s://%s/upload?presignedToken=%s", scheme, host, token)
	return token, uploadURL
}

func handlePresign(w http.ResponseWriter, r *http.Request) {
	log.Println("handlePresign: request received")
	if r.Header.Get("X-API-KEY") != apiKey {
		log.Println("handlePresign: unauthorized request, invalid API key")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ip := clientIP(r)
	if ok, retryAfter := presignLimiter.allow(ip); !ok {
		log.Printf("handlePresign: rate-limited IP %s (retry in %s)", ip, retryAfter)
		writeRateLimitError(w, retryAfter)
		return
	}

	authToken := r.URL.Query().Get("authToken")
	token, uploadURL := issueUploadToken(r.Host, authToken)
	log.Printf("handlePresign: generated token %s for IP %s authToken=%q", token, ip, authToken)
	log.Printf("handlePresign: returning upload URL %s", uploadURL)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"uploadURL": uploadURL,
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	log.Println("handleUpload: request received")
	token := r.URL.Query().Get("presignedToken")
	if token == "" {
		log.Println("handleUpload: missing presigned token")
		http.Error(w, "Missing presigned token", http.StatusUnauthorized)
		return
	}
	log.Printf("handleUpload: using token %s", token)

	rawEntry, ok := presignedTokens.Load(token)
	if !ok {
		log.Printf("handleUpload: token %s invalid or expired", token)
		http.Error(w, "Invalid or expired presigned token", http.StatusUnauthorized)
		return
	}
	entry, _ := rawEntry.(presignedTokenEntry)
	if !entry.expiry.IsZero() && time.Now().After(entry.expiry) {
		presignedTokens.Delete(token)
		log.Printf("handleUpload: token %s expired", token)
		http.Error(w, "Presigned token expired", http.StatusUnauthorized)
		return
	}
	presignedTokens.Delete(token)
	log.Printf("handleUpload: token %s validated (authToken=%q) and consumed", token, entry.authToken)

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if strings.Contains(err.Error(), "request body too large") || strings.Contains(err.Error(), "http: request body too large") {
			log.Printf("handleUpload: file exceeds %d bytes from IP %s", maxUploadBytes, clientIP(r))
			writeFileTooLargeError(w)
			return
		}
		log.Printf("handleUpload: error parsing form data: %v", err)
		http.Error(w, "Failed to parse form data", http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		log.Printf("handleUpload: error retrieving file: %v", err)
		http.Error(w, "Missing file in request", http.StatusBadRequest)
		return
	}
	defer file.Close()
	log.Printf("handleUpload: received file %s", handler.Filename)

	var (
		videoID   string
		authToken string
	)

	if entry.authToken != "" {
		// MCP path: row was created by start_upload, write into it.
		v, lerr := lookupVideoByAuthToken(entry.authToken)
		if lerr != nil {
			log.Printf("handleUpload: lookup by authToken %s failed: %v", entry.authToken, lerr)
			http.Error(w, "Bound video not found", http.StatusNotFound)
			return
		}
		if v.Status != "pending_upload" {
			log.Printf("handleUpload: bound video %s already in status %s", v.VideoID, v.Status)
			http.Error(w, "Bound video is not awaiting upload", http.StatusConflict)
			return
		}
		videoID = v.VideoID
		authToken = entry.authToken
	} else {
		// Webform path: no binding — generate a fresh row. whisperModel is
		// left empty so src/srt/main.go falls back to SUBTITLESKING_WHISPER_MODEL
		// (the deploy-wide default).
		videoID = generateVideoID()
		authToken, err = db.AddVideoToDatabase(handler.Filename, videoID, "", "new", "")
		if err != nil {
			log.Printf("AddVideoToDatabase error: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		log.Printf("handleUpload: created videoID %s authToken %s (webform path)", videoID, authToken)
	}

	uploadDir := filepath.Join("files", videoID, "uploads")
	if mkErr := os.MkdirAll(uploadDir, 0755); mkErr != nil {
		log.Printf("handleUpload: error creating directory %s: %v", uploadDir, mkErr)
		http.Error(w, "Failed creating upload directory", http.StatusInternalServerError)
		return
	}

	dstPath := filepath.Join(uploadDir, handler.Filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		log.Printf("handleUpload: error creating file at %s: %v", dstPath, err)
		http.Error(w, "Failed creating file on server", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, copyErr := io.Copy(dst, file); copyErr != nil {
		log.Printf("handleUpload: error copying file data: %v", copyErr)
		http.Error(w, "Error saving uploaded file", http.StatusInternalServerError)
		return
	}
	log.Printf("handleUpload: file %s saved successfully at %s", handler.Filename, dstPath)

	if entry.authToken != "" {
		// Flip pending_upload → new + record the actual filename (start_upload
		// only knew the name from the agent — trust the multipart filename).
		if err := markBoundUploadReady(videoID, handler.Filename); err != nil {
			log.Printf("handleUpload: markBoundUploadReady error: %v", err)
			http.Error(w, "Failed to record upload", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "success",
		"message":   "Upload successful",
		"videoID":   videoID,
		"authToken": authToken,
	})
	log.Println("handleUpload: response sent")
}

// lookupVideoByAuthToken reads a video row by its 8-digit auth token.
func lookupVideoByAuthToken(authToken string) (types.Video, error) {
	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return types.Video{}, err
	}
	defer dbConn.Close()

	var v types.Video
	err = dbConn.QueryRow(
		`SELECT video_id, file, username, status, auth_token, created_at, updated_at
		 FROM videos WHERE auth_token = ?`, authToken,
	).Scan(&v.VideoID, &v.File, &v.Username, &v.Status, &v.AuthToken, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return types.Video{}, err
	}
	return v, nil
}

// markBoundUploadReady flips a pending_upload row to "new" once bytes have
// arrived, and updates the file column to the actual uploaded filename.
func markBoundUploadReady(videoID, filename string) error {
	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return err
	}
	defer dbConn.Close()
	_, err = dbConn.Exec(
		`UPDATE videos SET status = 'new', file = ?, updated_at = ? WHERE video_id = ?`,
		filename, time.Now().Format(time.RFC3339), videoID,
	)
	return err
}

func handleQueue(w http.ResponseWriter, r *http.Request) {
	log.Println("handleQueue: request received")

	// Extract authToken from the request (e.g., as a query param).
	var reqBody struct {
		AuthToken string `json:"authToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		log.Printf("handleQueue: error decoding JSON body: %v", err)
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	authToken := reqBody.AuthToken
	if authToken == "" {
		log.Println("handleQueue: missing authToken")
		http.Error(w, "Missing authToken in request", http.StatusBadRequest)
		return
	}

	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Printf("handleQueue: error opening database: %v", err)
		http.Error(w, "Failed to open database", http.StatusInternalServerError)
		return
	}
	defer dbConn.Close()
	log.Println("handleQueue: database opened successfully")

	rows, err := dbConn.Query("SELECT auth_token, status, video_id, created_at FROM videos WHERE auth_token = ?", authToken)
	if err != nil {
		log.Printf("handleQueue: error querying database: %v", err)
		http.Error(w, "Failed to query database", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var files []map[string]any
	for rows.Next() {
		var authTokenValue, status, videoID, createdAt string
		if err := rows.Scan(&authTokenValue, &status, &videoID, &createdAt); err != nil {
			log.Printf("handleQueue: error scanning row: %v", err)
			http.Error(w, "Failed to scan row", http.StatusInternalServerError)
			return
		}
		scheme := "https"
		if strings.HasPrefix(r.Host, "localhost") {
			scheme = "http"
		}
		token := generateDownloadTokenForVideo(videoID)
		downloadURL := fmt.Sprintf("%s://%s/download?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)

		transcriptURL := ""
		if transcriptReady(status) {
			transcriptURL = fmt.Sprintf("%s://%s/transcript?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)
		}

		files = append(files, map[string]any{
			"authToken":     authTokenValue,
			"status":        status,
			"downloadURL":   downloadURL,
			"transcriptURL": transcriptURL,
			"queuePosition": queuePositionAhead(dbConn, status, createdAt),
		})
		log.Printf("handleQueue: queued file - authToken: %s, videoID: %s, status: %s", authTokenValue, videoID, status)
	}
	if len(files) == 0 {
		files = append(files, map[string]any{
			"authToken":     "",
			"status":        "404",
			"downloadURL":   "",
			"transcriptURL": "",
			"queuePosition": 0,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(files); err != nil {
		log.Printf("handleQueue: error encoding response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
	log.Println("handleQueue: response sent for queue")
}

func handleDownloadPresign(w http.ResponseWriter, r *http.Request) {
	log.Println("handleDownloadPresign: request received")
	if r.Header.Get("X-API-KEY") != apiKey {
		log.Println("handleDownloadPresign: unauthorized request, invalid API key")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// Require a videoID parameter to generate a URL for a specific video.
	videoID := r.URL.Query().Get("videoID")
	if videoID == "" {
		http.Error(w, "Missing videoID parameter", http.StatusBadRequest)
		return
	}

	// Generate a one-time token and store it along with the videoID.
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	downloadTokens.Store(token, videoID)
	log.Printf("handleDownloadPresign: generated download token %s for videoID %s", token, videoID)

	env := os.Getenv("SUBTITLESKING_ENV")
	scheme := "https"
	if env != "production" {
		scheme = "http"
	}

	downloadURL := fmt.Sprintf("%s://%s/download?videoID=%s&presignedToken=%s", scheme, r.Host, videoID, token)
	log.Printf("handleDownloadPresign: returning download URL %s", downloadURL)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"downloadURL": downloadURL,
	})
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	log.Println("handleDownload: request received")
	videoID := r.URL.Query().Get("videoID")
	token := r.URL.Query().Get("presignedToken")
	if videoID == "" || token == "" {
		http.Error(w, "Missing videoID or presigned token", http.StatusBadRequest)
		return
	}

	// Validate the token and check its expiry.
	val, ok := downloadTokens.Load(token)
	if !ok {
		http.Error(w, "Invalid or expired presigned token", http.StatusUnauthorized)
		return
	}
	entry, ok := val.(downloadTokenEntry)
	if !ok || entry.videoID != videoID {
		http.Error(w, "Token does not match the requested video", http.StatusUnauthorized)
		return
	}
	if time.Now().After(entry.expiry) {
		http.Error(w, "Presigned token expired", http.StatusUnauthorized)
		return
	}

	// Retrieve the video metadata from the database.
	video, err := getVideoByID(videoID)
	if err != nil {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	}

	// Construct the file path. (Assuming uploads are stored at files/<videoID>/uploads/<filename>)
	filePath := filepath.Join("files", videoID, "finished", video.File)
	log.Printf("handleDownload: serving file %s", filePath)
	http.ServeFile(w, r, filePath)
}

func generateVideoID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// queuePositionAhead returns how many other videos are ahead of this one
// in the processing pipeline. 0 means this video is next (or already done).
// "Ahead" = videos with an earlier created_at that are still pending
// (any status other than subtitles_burned, deleted, or error_*).
func queuePositionAhead(db *sql.DB, status, createdAt string) int {
	if status == "subtitles_burned" || status == "deleted" || status == "pending_upload" || strings.HasPrefix(status, "error_") {
		return 0
	}
	var count int
	row := db.QueryRow(
		`SELECT COUNT(*) FROM videos
		 WHERE created_at < ?
		   AND status != 'subtitles_burned'
		   AND status != 'deleted'
		   AND status != 'pending_upload'
		   AND status NOT LIKE 'error_%'`,
		createdAt,
	)
	if err := row.Scan(&count); err != nil {
		return 0
	}
	return count
}

// transcriptReady reports whether the SRT transcript file has been written
// to disk for a video at the given pipeline status. Includes
// error_while_burning_subtitles because the SRT is generated *before*
// the burn step — if burn fails (e.g. ffmpeg without libass), the
// transcript is still complete and useful.
func transcriptReady(status string) bool {
	switch status {
	case "srt_generated",
		"burning_subtitles",
		"subtitles_burned",
		"error_while_burning_subtitles":
		return true
	}
	return false
}

// transcriptPath returns the on-disk SRT path for a video. Mirrors the path
// used by the orchestrator in main.go (basename without extension + .srt).
func transcriptPath(videoID, file string) string {
	baseName := strings.TrimSuffix(file, filepath.Ext(file))
	return filepath.Join("files", videoID, "srt", baseName+".srt")
}

// handleTranscript serves the SRT transcript file. Auth uses the same
// presigned token mechanism as /download.
func handleTranscript(w http.ResponseWriter, r *http.Request) {
	log.Println("handleTranscript: request received")
	videoID := r.URL.Query().Get("videoID")
	token := r.URL.Query().Get("presignedToken")
	if videoID == "" || token == "" {
		http.Error(w, "Missing videoID or presigned token", http.StatusBadRequest)
		return
	}

	val, ok := downloadTokens.Load(token)
	if !ok {
		http.Error(w, "Invalid or expired presigned token", http.StatusUnauthorized)
		return
	}
	entry, ok := val.(downloadTokenEntry)
	if !ok || entry.videoID != videoID {
		http.Error(w, "Token does not match the requested video", http.StatusUnauthorized)
		return
	}
	if time.Now().After(entry.expiry) {
		http.Error(w, "Presigned token expired", http.StatusUnauthorized)
		return
	}

	video, err := getVideoByID(videoID)
	if err != nil {
		http.Error(w, "Video not found", http.StatusNotFound)
		return
	}

	srtPath := transcriptPath(videoID, video.File)
	if _, err := os.Stat(srtPath); err != nil {
		http.Error(w, "Transcript not yet available", http.StatusNotFound)
		return
	}

	baseName := strings.TrimSuffix(video.File, filepath.Ext(video.File))
	w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.srt"`, baseName))
	log.Printf("handleTranscript: serving %s", srtPath)
	http.ServeFile(w, r, srtPath)
}

func getVideoByID(videoID string) (types.Video, error) {
	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return types.Video{}, err
	}
	defer dbConn.Close()

	var video types.Video
	query := `SELECT video_id, file, username, status, created_at, updated_at FROM videos WHERE video_id = ?`
	err = dbConn.QueryRow(query, videoID).Scan(&video.VideoID, &video.File, &video.Username, &video.Status, &video.CreatedAt, &video.UpdatedAt)
	if err != nil {
		return types.Video{}, err
	}
	return video, nil
}
