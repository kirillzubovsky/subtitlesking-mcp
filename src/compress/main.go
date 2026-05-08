package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"

	_ "github.com/mattn/go-sqlite3"
)

// ffmpegCompressVideo compresses a video file using FFMPEG. ffmpeg's
// stderr is forwarded to this process's stderr so failures surface in
// the systemd journal (otherwise the orchestrator only sees "exit
// status 1" and there is no way to tell encoding errors from missing
// codecs from a missing input file).
func ffmpegCompressVideo(inputFile, outputFile string, maxWidth int, preset string, crf int) error {
	var args []string

	args = append(args, "-i", inputFile)
	args = append(args, "-vf", fmt.Sprintf("scale='min(%d,iw):-2'", maxWidth))
	args = append(args, "-c:v", "libx264", "-crf", fmt.Sprintf("%d", crf), "-preset", preset)
	args = append(args, "-c:a", "aac", "-b:a", "128k")
	args = append(args, "-movflags", "+faststart")
	args = append(args, outputFile)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func main() {
	maxWidthFlag := flag.Int("max_width", 1280, "Maximum width for compressed video")
	presetFlag := flag.String("preset", "medium", "FFMPEG compression preset")
	crfFlag := flag.Int("crf", 23, "Constant Rate Factor for compression quality")
	flag.Parse()

	// Expect the first argument to be the video ID
	if len(flag.Args()) < 1 {
		fmt.Println("Usage:", os.Args[0], "<video_id> [max_width] [preset] [crf]")
		os.Exit(1)
	}

	videoID := flag.Arg(0)

	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		log.Fatalf("Error opening DB: %v\n", err)
	}
	defer db.Close()

	// Lookup the file name by the supplied videoID
	var fileName string
	if err = db.QueryRow("SELECT file FROM videos WHERE video_id = ?", videoID).Scan(&fileName); err != nil {
		log.Fatalf("Could not find file for video_id=%s: %v\n", videoID, err)
	}

	maxWidth := *maxWidthFlag

	// Now we look inside files/<videoID>/uploads/<fileName> for the source
	inputFile := filepath.Join("files", videoID, "uploads", fileName)

	// Create a subdirectory for the compressed output
	compressedDir := filepath.Join("files", videoID, "compressed")
	if err := os.MkdirAll(compressedDir, 0755); err != nil {
		log.Fatalf("Could not create compressed directory for videoID=%s: %v\n", videoID, err)
	}

	// Output goes to files/<videoID>/compressed/<fileName>
	outputFile := filepath.Join(compressedDir, fileName)

	// If the compressed file already exists, skip
	if _, err := os.Stat(outputFile); err == nil {
		fmt.Printf("Compressed file already exists at: %s\nSkipping compression.\n", outputFile)
		os.Exit(0)
	}

	// Ensure the input file exists
	if _, err := os.Stat(inputFile); os.IsNotExist(err) {
		fmt.Println("Run video compression: Input file does not exist:", inputFile)
		os.Exit(1)
	}

	// Call ffmpegCompressVideo
	if err := ffmpegCompressVideo(inputFile, outputFile, maxWidth, *presetFlag, *crfFlag); err != nil {
		fmt.Println("Run video compression: Error compressing video:", err)
		os.Exit(1)
	}

	fmt.Println("Video compression completed successfully. Saved to:", outputFile)
}
