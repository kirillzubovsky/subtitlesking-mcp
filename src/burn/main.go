package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:", os.Args[0], "<video_id>")
		os.Exit(1)
	}

	videoID := os.Args[1]

	// 1) Open the database
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Fatalf("Error opening DB: %v\n", err)
	}
	defer db.Close()

	// 2) Lookup the file name by the supplied videoID
	var fileName string
	if err = db.QueryRow("SELECT file FROM videos WHERE video_id = ?", videoID).Scan(&fileName); err != nil {
		log.Fatalf("Could not find file for video_id=%s: %v\n", videoID, err)
	}

	// 3) Build the compressed input file path: files/<videoID>/compressed/<fileName>
	inputFile := filepath.Join("files", videoID, "uploads", fileName)
	if _, err := os.Stat(inputFile); os.IsNotExist(err) {
		fmt.Println("Burn subtitles: Input file does not exist:", inputFile)
		os.Exit(1)
	}

	// 4) Figure out the matching .srt file: files/<videoID>/srt/<baseName>.srt
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	srtFile := filepath.Join("files", videoID, "srt", baseName+".srt")
	if _, err := os.Stat(srtFile); os.IsNotExist(err) {
		fmt.Println("Burn subtitles: Subtitle file does not exist:", srtFile)
		os.Exit(1)
	}

	// 5) Prepare the finished directory: files/<videoID>/finished
	outputDir := filepath.Join("files", videoID, "finished")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Println("Burn subtitles: Could not create 'finished' directory:", err)
		os.Exit(1)
	}

	// Construct the outputFile path
	outputFile := filepath.Join(outputDir, fileName)
	if _, err := os.Stat(outputFile); err == nil {
		fmt.Printf("Burn subtitles: Output file already exists at: %s\nSkipping subtitle burning.\n", outputFile)
		os.Exit(0)
	}

	// 6) Run ffmpeg with quiet logging and force output frame rate to 60 fps
	// Escape special characters in the SRT path for FFMPEG
	escapedSrtPath := fmt.Sprintf("'%s'", strings.ReplaceAll(srtFile, "'", "\\'"))

	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-i", inputFile,
		"-vf", fmt.Sprintf("subtitles=%s:force_style='Fontsize=12,BorderStyle=3,PrimaryColour=&HFFFFFF&,BackColour=&H000000&,MarginV=10'", escapedSrtPath),
		"-r", "60",
		outputFile)
	// Capture both stdout and stderr
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Printf("Burn subtitles: Error running FFMPEG for video %s.\nInput: %s\nSRT: %s\nOutput: %s\nError: %v\n",
			videoID, inputFile, srtFile, outputFile, err)
		os.Exit(1)
	}

	fmt.Println("Burning subtitles completed. File saved in:", outputFile)
}
