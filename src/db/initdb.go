package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// InitDB creates the videos table if it doesn't exist and runs any
// idempotent column-add migrations needed for older databases.
func InitDB(databasePath string) error {
	db, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		return fmt.Errorf("InitDB: failed to open DB: %w", err)
	}
	defer db.Close()

	createVideosTableSQL := `CREATE TABLE IF NOT EXISTS videos (
		video_id TEXT PRIMARY KEY,
		file TEXT,
		username TEXT,
		status TEXT,
		auth_token TEXT,
		whisper_model TEXT DEFAULT '',
		created_at TEXT,
		updated_at TEXT
	);`

	if _, err = db.Exec(createVideosTableSQL); err != nil {
		return fmt.Errorf("InitDB: failed to create table: %w", err)
	}

	// Migrate existing databases that predate the whisper_model column.
	// SQLite's ALTER TABLE ADD COLUMN errors with "duplicate column name"
	// when run twice — that's the success signal here, so we ignore it.
	if _, err := db.Exec(`ALTER TABLE videos ADD COLUMN whisper_model TEXT DEFAULT ''`); err != nil {
		// Only surface non-duplicate errors. SQLite phrases the duplicate
		// case as: "duplicate column name: whisper_model".
		// Anything else (locked db, syntax error, etc.) is a real problem.
		// Using a substring check rather than parsing the typed sqlite3
		// error so we don't depend on the driver's internals.
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("InitDB: add whisper_model column: %w", err)
		}
	}

	return nil
}
