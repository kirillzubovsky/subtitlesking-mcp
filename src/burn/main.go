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

// preflightFFmpegHasSubtitlesFilter checks that the local ffmpeg build
// includes the `subtitles` filter (which requires --enable-libass at
// build time). The default `brew install ffmpeg` formula on macOS does
// NOT include libass — users get a build that compresses fine but
// cannot burn subtitles. Detect this up-front and fail with an
// actionable message instead of letting ffmpeg exit 234 with a cryptic
// "Error parsing filter description" message.
func preflightFFmpegHasSubtitlesFilter() error {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not run `ffmpeg -filters`: %w (is ffmpeg installed?)", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Output rows look like "TS. subtitles V->V Apply subtitles…".
		// We just need the second whitespace-separated field == "subtitles".
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "subtitles" {
			return nil
		}
	}
	return fmt.Errorf(
		"ffmpeg is installed but lacks the `subtitles` filter — your build was compiled without libass. " +
			"On macOS, the default `brew install ffmpeg` formula no longer ships libass. Install the " +
			"libass-enabled build with:\n\n" +
			"    brew tap homebrew-ffmpeg/ffmpeg\n" +
			"    brew install homebrew-ffmpeg/ffmpeg/ffmpeg\n\n" +
			"Verify with:  ffmpeg -filters 2>&1 | grep -E '\\bsubtitles\\b'",
	)
}

// escapeFFmpegFilterPath escapes the characters that ffmpeg's filter
// description parser treats as syntax: backslash, colon (option separator),
// comma (filter separator), single quote, and brackets (filterchain
// labels). Backslash must be replaced first so we don't double-escape
// the escape character we add for the others.
//
// Filter description grammar:
//   filterchain = filter [, filter]*
//   filter      = name [= options]
//   options     = opt [: opt]*
//   opt         = key=value
// So `:` and `,` in a path turn into syntax errors unless escaped.
//
// This is filter-level escaping — distinct from shell escaping (which
// we don't need here because exec.Command bypasses the shell).
func escapeFFmpegFilterPath(p string) string {
	// Order matters: backslash first.
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	p = strings.ReplaceAll(p, `,`, `\,`)
	p = strings.ReplaceAll(p, `[`, `\[`)
	p = strings.ReplaceAll(p, `]`, `\]`)
	return p
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:", os.Args[0], "<video_id>")
		os.Exit(1)
	}

	if err := preflightFFmpegHasSubtitlesFilter(); err != nil {
		fmt.Fprintln(os.Stderr, "Burn subtitles: preflight failed.")
		fmt.Fprintln(os.Stderr, err)
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

	// 6) Run ffmpeg. Build the filter description carefully:
	//   - The SRT path goes through filter-level escaping (no shell here,
	//     so we must NOT wrap in literal '…'; ffmpeg ≥7 treats those as
	//     part of the option value and chokes).
	//   - force_style is a single string of comma-separated key=value
	//     pairs. Once again, no surrounding quotes — exec.Command passes
	//     the value literally to ffmpeg.
	//
	// Frame rate forced to 60 fps to keep the burned-in text crisp on
	// high-fps source playback.
	escapedSrtPath := escapeFFmpegFilterPath(srtFile)
	const forceStyle = "Fontsize=12,BorderStyle=3,PrimaryColour=&HFFFFFF&,BackColour=&H000000&,MarginV=10"
	filterDesc := fmt.Sprintf("subtitles=%s:force_style=%s", escapedSrtPath, forceStyle)

	cmd := exec.Command("ffmpeg", "-loglevel", "error", "-i", inputFile,
		"-vf", filterDesc,
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
