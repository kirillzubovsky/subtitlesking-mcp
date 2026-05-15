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

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"

	_ "github.com/mattn/go-sqlite3"
)

// Subtitle-chunking defaults. Whisper's "natural" segments are paragraph-
// shaped (long, narrative). For burned-in subtitles you want broadcast-
// style chunks that update every few seconds. These values yield ~3–5
// second segments at typical speech rates.
const (
	defaultMaxLineWidth = 42 // chars per line; 42 is the broadcast-safe norm
	defaultMaxLineCount = 2  // max lines per displayed chunk
)

// envIntPositive reads a positive integer from env. Returns fallback for
// unset, zero, negative, or unparseable values — defensive against typos
// in deploy config that should not stall the pipeline.
func envIntPositive(name string, fallback int) int {
	if s := strings.TrimSpace(os.Getenv(name)); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// validWhisperModels is the set of model names whisper-cli accepts. We
// allow common aliases but not arbitrary strings — passing junk to
// whisper would make it print a usage error and exit 1, which the
// orchestrator can't disambiguate from a real failure.
var validWhisperModels = map[string]bool{
	"tiny": true, "tiny.en": true,
	"base": true, "base.en": true,
	"small": true, "small.en": true,
	"medium": true, "medium.en": true,
	"large": true, "large-v1": true, "large-v2": true, "large-v3": true, "large-v3-turbo": true,
}

// largeModelAlias is the concrete whisper model we substitute when the user
// (or env var) asks for "large" or "large-v3". The turbo variant is ~1.6GB
// instead of ~3GB, ~8× faster on CPU, with marginal English accuracy loss —
// the right default for our use case. Users who explicitly want the older
// model can pass "large-v3-turbo" or any other specific variant; only the
// generic alias is redirected.
const largeModelAlias = "large-v3-turbo"

// resolveWhisperModel picks the whisper model name to invoke for one
// transcription. Precedence:
//  1. row-level override (set by the MCP `quality` tool param)
//  2. SUBTITLESKING_WHISPER_MODEL env var (deploy-wide default)
//  3. "medium" (sensible balance of quality/speed on CPU)
// Unknown values fall through to the next layer instead of erroring,
// so a typo in an env var doesn't stall the pipeline.
// Generic "large"/"large-v3" requests are aliased to large-v3-turbo.
func resolveWhisperModel(rowOverride string) string {
	for _, candidate := range []string{
		strings.TrimSpace(rowOverride),
		strings.TrimSpace(os.Getenv("SUBTITLESKING_WHISPER_MODEL")),
	} {
		if candidate != "" && validWhisperModels[candidate] {
			if candidate == "large" || candidate == "large-v3" {
				return largeModelAlias
			}
			return candidate
		}
	}
	return "medium"
}

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

	// 2) Lookup the file name + per-row model override.
	// whisper_model is empty for legacy rows / webform uploads — we fall
	// back to the SUBTITLESKING_WHISPER_MODEL env var, then to "medium".
	var (
		fileName    string
		rowModel    string
	)
	if err = db.QueryRow(
		"SELECT file, COALESCE(whisper_model, '') FROM videos WHERE video_id = ?",
		videoID,
	).Scan(&fileName, &rowModel); err != nil {
		log.Fatalf("Could not find file for video_id=%s: %v\n", videoID, err)
	}

	model := resolveWhisperModel(rowModel)
	fmt.Printf("Get SRT: using whisper model %q for video_id=%s\n", model, videoID)

	// 3) Build the input/output paths inside files/<videoID> subfolders
	inputFile := filepath.Join("files", videoID, "compressed", fileName)
	srtDir := filepath.Join("files", videoID, "srt")
	srtFile := filepath.Join(srtDir, fileName+".srt")

	// Ensure the input file exists
	if _, err := os.Stat(inputFile); os.IsNotExist(err) {
		fmt.Println("Get SRT: Input file does not exist:", inputFile)
		os.Exit(1)
	}

	// Ensure the SRT directory exists
	if err := os.MkdirAll(srtDir, 0755); err != nil {
		fmt.Println("Get SRT: Could not create srt directory:", err)
		os.Exit(1)
	}

	// Check if SRT file already exists
	if _, err := os.Stat(srtFile); err == nil {
		fmt.Printf("Get SRT: SRT file already exists at: %s\nSkipping transcription.\n", srtFile)
		os.Exit(0)
	}

	// 4) Subtitle chunking knobs. SUBTITLESKING_SRT_MAX_LINE_WIDTH and
	//    SUBTITLESKING_SRT_MAX_LINE_COUNT control segment shape; the optional
	//    SUBTITLESKING_SRT_MAX_WORDS_PER_LINE produces TikTok-style short
	//    bursts (1–5 words on screen at a time). All require word-level
	//    timestamps from whisper.
	maxLineWidth := envIntPositive("SUBTITLESKING_SRT_MAX_LINE_WIDTH", defaultMaxLineWidth)
	maxLineCount := envIntPositive("SUBTITLESKING_SRT_MAX_LINE_COUNT", defaultMaxLineCount)
	maxWordsPerLine := envIntPositive("SUBTITLESKING_SRT_MAX_WORDS_PER_LINE", 0)

	// 5) Prepare and run the Whisper command
	whisperArgs := []string{
		inputFile,
		"--model", model,
		"--output_format", "srt",
		"--output_dir", srtDir,
		"--word_timestamps", "True",
	}
	if maxWordsPerLine > 0 {
		// Word-cap mode: --max_words_per_line is mutually exclusive with
		// --max_line_width / --max_line_count in whisper's CLI.
		whisperArgs = append(whisperArgs, "--max_words_per_line", strconv.Itoa(maxWordsPerLine))
	} else {
		whisperArgs = append(whisperArgs,
			"--max_line_width", strconv.Itoa(maxLineWidth),
			"--max_line_count", strconv.Itoa(maxLineCount),
		)
	}

	fmt.Printf("Get SRT: whisper args: model=%s max_line_width=%d max_line_count=%d max_words_per_line=%d\n",
		model, maxLineWidth, maxLineCount, maxWordsPerLine)

	cmd := exec.Command("whisper", whisperArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command
	if err := cmd.Run(); err != nil {
		fmt.Println("Get SRT: Error running Whisper:", err)
		os.Exit(1)
	}

	fmt.Println("Transcription completed. SRT file saved in:", srtFile)
}
