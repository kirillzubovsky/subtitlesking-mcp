package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/db"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/server"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/utils"

	_ "github.com/mattn/go-sqlite3"
)

// maxWorkers controls how many videos process concurrently in the
// pipeline (compress + Whisper + burn). Whisper-large is the heavy
// step; tune this to your CPU/GPU budget. Default 2.
//
// Env var is prefixed because the VPS is shared with other services
// (md2doc, scoutzie, transcriptking) — generic names like MAX_WORKERS
// risk a future cross-service collision.
func maxWorkers() int {
	if s := os.Getenv("SUBTITLESKING_MAX_WORKERS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

var (
	videos []types.Video // Ensure this is declared at the package level
	mutex  sync.Mutex    // Mutex for thread-safe access to videos

	// processing tracks video IDs currently in flight so a re-scan does
	// not double-pick a video that is already being worked on.
	processing   = make(map[string]bool)
	processingMu sync.Mutex
)

func claim(videoID string) bool {
	processingMu.Lock()
	defer processingMu.Unlock()
	if processing[videoID] {
		return false
	}
	processing[videoID] = true
	return true
}

func release(videoID string) {
	processingMu.Lock()
	defer processingMu.Unlock()
	delete(processing, videoID)
}

func main() {
	log.Println("main.go started")

	// Always run InitDB on startup. It's idempotent — CREATE TABLE IF NOT
	// EXISTS for fresh installs, and ALTER TABLE ADD COLUMN that swallows
	// "duplicate column name" so re-running on a populated DB is a no-op.
	// Doing this unconditionally is what makes schema migrations
	// (whisper_model, future columns) actually reach long-lived production
	// databases — the previous "skip if file present" guard meant migrations
	// silently never ran on any deploy after the first one.
	if err := db.InitDB(types.DatabasePath); err != nil {
		log.Fatalf("InitDB failed: %v", err)
	}
	log.Println("Database initialized / migrated.")

	// Instead of running an external process, call Server Start once:
	log.Println("main: starting embedded upload server")
	go server.StartUploadServer()

	// Load existing processed files with their statuses
	var err error
	videos, err = loadProcessedFiles() // Load videos into the package-level variable
	if err != nil {
		log.Fatal("Error loading processed files:", err)
	}

	log.Printf("Loaded %d videos from %s\n", len(videos), types.DatabasePath)

	// Start cleanup ticker: run cleanupOldVideos() every hour, and prune
	// abandoned pending_upload rows (slot was claimed but bytes never
	// arrived) every 15 minutes.
	go func() {
		tickerOld := time.NewTicker(1 * time.Hour)
		tickerPending := time.NewTicker(15 * time.Minute)
		defer tickerOld.Stop()
		defer tickerPending.Stop()
		for {
			select {
			case <-tickerOld.C:
				log.Println("Running hourly cleanup of old videos...")
				cleanupOldVideos()
			case <-tickerPending.C:
				log.Println("Running cleanup of stale pending_upload rows...")
				cleanupPendingUploads()
			}
		}
	}()

	// Bounded worker pool. Up to maxWorkers() videos process in parallel.
	workers := maxWorkers()
	log.Printf("main: starting worker pool with %d workers", workers)
	slots := make(chan struct{}, workers)

	processOne := func(v types.Video) {
		defer release(v.VideoID)
		log.Printf("worker: processing %s (file=%s)", v.VideoID, v.File)

		if err := compressVideo(v); err != nil {
			log.Println("Error during video compression:", err)
			return
		}
		if err := generateSRT(v); err != nil {
			log.Println("Error during SRT generation:", err)
			return
		}
		if err := burnSubtitles(v); err != nil {
			log.Println("Error during subtitle burning:", err)
			return
		}
	}

	scanDir := func() {
		mutex.Lock()
		current, lerr := loadProcessedFiles()
		if lerr != nil {
			mutex.Unlock()
			log.Println("Error reloading processed files:", lerr)
			return
		}
		videos = current
		mutex.Unlock()

		for _, v := range current {
			if v.Status != "new" {
				continue
			}
			if !claim(v.VideoID) {
				continue
			}
			vid := v
			slots <- struct{}{} // blocks if pool is full
			go func() {
				defer func() { <-slots }()
				processOne(vid)
			}()
		}
	}

	// Initial scan
	log.Println("Initial scan for new uploads...")
	scanDir()

	// Scan every 1 minute
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("Scanning for new uploads...")
		scanDir()
	}
}

// Load processed files from the SQLite database
func loadProcessedFiles() ([]types.Video, error) {
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT video_id, file, username, status, auth_token, created_at, updated_at FROM videos")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var videos []types.Video
	for rows.Next() {
		var video types.Video
		if err := rows.Scan(&video.VideoID, &video.File, &video.Username, &video.Status, &video.AuthToken, &video.CreatedAt, &video.UpdatedAt); err != nil {
			return nil, err
		}
		videos = append(videos, video)
	}

	return videos, nil
}

// Update the status of a video in the SQLite database with retry logic
func updateVideoStatus(video types.Video) error {
	const maxRetries = 3
	var err error

	for i := 0; i < maxRetries; i++ {
		err = attemptUpdateTx(video)
		if err == nil {
			return nil
		}
		// Instead of type asserting to sqlite3.Error, check the error text
		if err != nil && strings.Contains(err.Error(), "locked") {
			time.Sleep(time.Duration(i+1) * time.Second)
			continue
		}
		return err
	}
	return fmt.Errorf("failed to update video status after %d attempts: %v", maxRetries, err)
}

// Use a transaction rather than a plain db.Exec
func attemptUpdateTx(video types.Video) error {
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	updateSQL := `UPDATE videos SET status = ?, updated_at = ? WHERE video_id = ?`
	_, err = tx.Exec(updateSQL, video.Status, time.Now().Format(time.RFC3339), video.VideoID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Compress the video
func compressVideo(video types.Video) error {
	log.Printf("Compressing video: %s\n", video.VideoID)

	video.Status = "compressing"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'compressing':", err)
	}

	// Create the subdirectory for compressed files: "files/<videoID>/compressed"
	compressedDir := filepath.Join("files", video.VideoID, "compressed")
	if err := os.MkdirAll(compressedDir, 0755); err != nil {
		return fmt.Errorf("failed to create compressed directory: %v", err)
	}

	compressedFile := filepath.Join(compressedDir, video.File)

	// Use the utility function to check if the SRT file exists
	if err := utils.CheckAndUpdateStatus(&video, compressedFile, "compressed", updateVideoStatus); err != nil {
		return err
	}

	cmd := exec.Command("go", "run", "src/compress/main.go", video.VideoID)
	// Forward subprocess stdout/stderr to the orchestrator's journal so
	// failures land in journalctl with the actual ffmpeg error message.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("compressVideo: subprocess failed for videoID=%s: %v", video.VideoID, err)
		video.Status = "error_while_compressing"
		updateVideoStatus(video)
		return err
	}

	// Update the video status to "compressed" after successful compression
	video.Status = "compressed"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'compressed':", err)
	}

	return nil
}

// Generate SRT file
func generateSRT(video types.Video) error {
	log.Printf("Generating SRT for video: %s\n", video.VideoID)

	video.Status = "generating_srt"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'generating_srt':", err)
	}

	// Create the subdirectory for SRT files: "files/<videoID>/srt"
	srtDir := filepath.Join("files", video.VideoID, "srt")
	if err := os.MkdirAll(srtDir, 0755); err != nil {
		return fmt.Errorf("failed to create srt directory: %v", err)
	}

	// Generate SRT file path using the video file name without its extension.
	baseName := strings.TrimSuffix(video.File, filepath.Ext(video.File))
	srtFile := filepath.Join(srtDir, baseName+".srt")

	// If the SRT file already exists, skip generation and return
	if _, err := os.Stat(srtFile); err == nil {
		log.Printf("SRT file already exists at: %s. Skipping SRT generation.", srtFile)
		return nil
	}

	cmd := exec.Command("go", "run", "src/srt/main.go", video.VideoID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the SRT generation command
	if err := cmd.Run(); err != nil {
		video.Status = "error_while_generating_srt"
		updateVideoStatus(video) // Update status in case of error
		return err
	}

	// Update the video status to "srt_generated" after successful generation
	video.Status = "srt_generated"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp

	// Save the SRT file path in the video record

	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'srt_generated':", err)
	}

	return nil
}

// Burn subtitles onto the video
func burnSubtitles(video types.Video) error {
	log.Printf("Burning subtitles onto video: %s\n", video.VideoID)

	video.Status = "burning_subtitles"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'burning_subtitles':", err)
	}

	// Create the subdirectory for finished files: "files/<videoID>/finished"
	finishedDir := filepath.Join("files", video.VideoID, "finished")
	if err := os.MkdirAll(finishedDir, 0755); err != nil {
		return fmt.Errorf("failed to create finishing directory: %v", err)
	}

	outputFile := filepath.Join(finishedDir, video.File)

	// Use the utility function to check if the output file exists
	if err := utils.CheckAndUpdateStatus(&video, outputFile, "subtitles_burned", updateVideoStatus); err != nil {
		return err
	}

	cmd := exec.Command("go", "run", "src/burn/main.go", video.VideoID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the subtitle burning command
	if err := cmd.Run(); err != nil {
		video.Status = "error_while_burning_subtitles"
		updateVideoStatus(video) // Update status in case of error
		return err
	}

	// Update the video status to "subtitles_burned" after successful burning
	video.Status = "subtitles_burned"
	video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
	if err := updateVideoStatus(video); err != nil {
		log.Println("Error updating video status to 'subtitles_burned':", err)
	}

	return nil
}

// cleanupPendingUploads removes "pending_upload" rows that were claimed via
// MCP `start_upload` but never received bytes. Bounds the table size against
// abuse and ensures abandoned auth_tokens don't linger on the user's screen.
// TTL of 1h matches the presigned upload URL's TTL.
func cleanupPendingUploads() {
	dbConn, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Println("cleanupPendingUploads: open db error:", err)
		return
	}
	defer dbConn.Close()

	cutoff := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	res, err := dbConn.Exec(
		`DELETE FROM videos WHERE status = 'pending_upload' AND created_at < ?`,
		cutoff,
	)
	if err != nil {
		log.Println("cleanupPendingUploads: delete error:", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("cleanupPendingUploads: pruned %d stale pending_upload rows", n)
	}
}

// cleanupOldVideos queries the database for videos created >24 hours ago,
// deletes their files directory, and updates their status to "deleted".
func cleanupOldVideos() {
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Println("Error opening database for cleanup:", err)
		return
	}
	defer db.Close()

	query := `SELECT video_id, file, username, status, auth_token, created_at, updated_at 
	          FROM videos WHERE status <> 'deleted'`
	rows, err := db.Query(query)
	if err != nil {
		log.Println("Error querying database for cleanup:", err)
		return
	}
	defer rows.Close()

	var expired []types.Video
	for rows.Next() {
		var v types.Video
		if err := rows.Scan(&v.VideoID, &v.File, &v.Username, &v.Status, &v.AuthToken, &v.CreatedAt, &v.UpdatedAt); err != nil {
			log.Println("Error scanning video row:", err)
			continue
		}
		created, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			log.Println("Error parsing created_at for video", v.VideoID, ":", err)
			continue
		}
		if time.Since(created) > 24*time.Hour {
			expired = append(expired, v)
		}
	}

	for _, v := range expired {
		dir := filepath.Join("files", v.VideoID)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("Failed to remove directory %s: %v", dir, err)
		} else {
			log.Printf("Removed directory %s", dir)
		}

		_, err := db.Exec(`UPDATE videos SET status = ?, updated_at = ? WHERE video_id = ?`,
			"deleted", time.Now().Format(time.RFC3339), v.VideoID)
		if err != nil {
			log.Printf("Failed to update status to deleted for video %s: %v", v.VideoID, err)
		} else {
			log.Printf("Updated video %s status to 'deleted'", v.VideoID)
		}
	}
}
