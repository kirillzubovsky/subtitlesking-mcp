package main

import (
	"log"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/db"
	"github.com/kirillzubovsky/subtitlesking-mcp/src/types"
)

// main() for command-line usage: run "go run setup.go"
func main() {
	SetupSystems()
}

func SetupSystems() {
	log.Println("Running setup to initialize DB...")
	if err := db.InitDB(types.DatabasePath); err != nil {
		log.Fatalf("Failed to initialize DB: %v", err)
	}
	log.Println("Database initialized successfully.")
}
