package cmd_cleanup

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/utils"

	_ "github.com/mattn/go-sqlite3"
)

func Cleanup() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: go run cleanup.go <video_id>")
		return
	}

	videoID := os.Args[1]

	// Load the video from the database
	video, err := getVideoByID(videoID)
	if err != nil {
		log.Println("Error retrieving video:", err)
		return
	}

	// Call the CleanupFiles function
	if err := utils.CleanupFiles(&video); err != nil {
		log.Println("Error cleaning up files:", err)
		return
	}

	log.Printf("Cleanup completed for video ID: %s\n", videoID)
}

// getVideoByID retrieves a video from the database by its ID
func getVideoByID(videoID string) (types.Video, error) {
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return types.Video{}, err
	}
	defer db.Close()

	var video types.Video
	query := `SELECT video_id, file, username, status, auth_token, created_at, updated_at FROM videos WHERE video_id = ?`
	err = db.QueryRow(query, videoID).Scan(&video.VideoID, &video.File, &video.Username, &video.Status, &video.AuthToken, &video.CreatedAt, &video.UpdatedAt)
	if err != nil {
		return types.Video{}, err
	}

	return video, nil
}
