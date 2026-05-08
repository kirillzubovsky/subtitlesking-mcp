package db

import (
	"database/sql"
	"fmt"
	"math/rand"
	"time"

	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"

	_ "github.com/mattn/go-sqlite3"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// AddVideoToDatabase adds a video entry and returns a unique 8-digit AuthToken.
// whisperModel is the SRT-stage model override for this row; pass "" to fall
// back to the SUBTITLESKING_WHISPER_MODEL env var (which itself defaults to
// "medium" inside src/srt/main.go).
func AddVideoToDatabase(file, videoID, username, status, whisperModel string) (string, error) {
	return addVideo(file, videoID, username, status, whisperModel)
}

func addVideo(file, videoID, username, status, whisperModel string) (string, error) {
	db, err := sql.Open("sqlite3", types.DatabasePath)
	if err != nil {
		return "", err
	}
	defer db.Close()

	token, err := generateAuthToken(db)
	if err != nil {
		return "", err
	}

	now := time.Now().Format(time.RFC3339)
	insertSQL := `INSERT INTO videos (video_id, file, username, status, auth_token, whisper_model, created_at, updated_at)
	              VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = db.Exec(insertSQL, videoID, file, username, status, token, whisperModel, now, now)
	if err != nil {
		return "", err
	}

	return token, nil
}

func generateAuthToken(db *sql.DB) (string, error) {
	for {
		// Generate a random 8-digit number between 10000000 and 99999999.
		token := fmt.Sprintf("%08d", rand.Intn(90000000)+10000000)
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM videos WHERE auth_token = ?", token).Scan(&count)
		if err != nil {
			return "", err
		}
		if count == 0 {
			return token, nil
		}
		// If token exists, try again.
	}
}
