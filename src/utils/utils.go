package utils

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"
)

// CheckAndUpdateStatus checks if a file exists and updates the video status accordingly.
func CheckAndUpdateStatus(video *types.Video, filePath string, statusIfExists string, dbUpdateFunc func(video types.Video) error) error {
	if _, err := os.Stat(filePath); err == nil {
		log.Printf("File already exists at: %s\nSkipping operation.\n", filePath)
		video.Status = statusIfExists                     // Update status to the specified status
		video.UpdatedAt = time.Now().Format(time.RFC3339) // Update the timestamp
		return dbUpdateFunc(*video)                       // Update the database status
	}
	return nil
}

// CleanupFiles cleans up the files after the operation is complete
func CleanupFiles(video *types.Video) error {
	// Define file paths
	inputFile := filepath.Join("files/uploads", video.File)
	compressedFile := filepath.Join("files/compressed", video.File)
	srtFile := filepath.Join("files/srt", video.File+".srt")

	// Remove the original video file
	if err := os.Remove(inputFile); err != nil {
		log.Println("Error deleting original video file:", err)
	}

	// Remove the compressed video file
	if err := os.Remove(compressedFile); err != nil {
		log.Println("Error deleting compressed video file:", err)
	}

	// Remove the SRT file
	if err := os.Remove(srtFile); err != nil {
		log.Println("Error deleting SRT file:", err)
	}

	return nil
}
