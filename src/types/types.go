package types

var DatabasePath = "videos.db" // Exported variable

// Video represents a video entry in the database
type Video struct {
	VideoID   string `json:"videoID"`
	File      string `json:"file"`
	Username  string `json:"username"`
	Status    string `json:"status"`
	AuthToken string `json:"authToken"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}
