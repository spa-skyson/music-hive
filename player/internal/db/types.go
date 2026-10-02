package db

import "time"

// Общие типы контракта Backend: SQL-реализация одна (*PGStore), но типы
// использует и верхний слой (api, index, playback, recommend).

// mondayZeroWeekday переводит Go-неделю (Sunday=0) в понедельник-ноль —
// так хранится weekday в listening_history.
func mondayZeroWeekday(day time.Weekday) int {
	return (int(day) + 6) % 7
}

type TrackRow struct {
	ID          int64
	Path        string
	Title       string
	Artist      string
	Album       string
	Duration    float64
	FileMD5     string
	CreatedAt   string
	ArtworkPath string
	ClusterID   int
	Embedding   []byte
	Dim         int
	Shown       int
	SkipEarly   int
	Completed   int
}

type CatalogTrack struct {
	ID       int64
	Path     string
	Title    string
	Artist   string
	Album    string
	Duration float64
	Artwork  string
	Cluster  int
	Status   string
}

type RecommendationImpression struct {
	SessionID     string
	TrackID       int64
	Position      int
	Score         float64
	CosineTaste   float64
	CosineCurrent float64
	Explore       bool
	NewBoost      bool
	Maturity      string
	Mode          string
}

type Job struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Payload   string `json:"payload_json,omitempty"`
	Result    string `json:"result_json,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Lyrics is plain/synced text for a track (filled by Python `music-hive lyrics`).
type Lyrics struct {
	TrackID      int64  `json:"track_id"`
	PlainLyrics  string `json:"plain_lyrics"`
	SyncedLyrics string `json:"synced_lyrics"`
	Source       string `json:"source"`
	SourceID     string `json:"source_id"`
	Instrumental bool   `json:"instrumental"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

type Metrics struct {
	Listens7d       int     `json:"listens_7d"`
	Skips7d         int     `json:"skips_7d"`
	SkipRate7d      float64 `json:"skip_rate_7d"`
	Completes7d     int     `json:"completes_7d"`
	ExploreShown    int     `json:"explore_shown"`
	ExploitShown    int     `json:"exploit_shown"`
	ExploreSkips    int     `json:"explore_early_skips"`
	ExploitSkips    int     `json:"exploit_early_skips"`
	ExploreComplete int     `json:"explore_completes"`
	ExploitComplete int     `json:"exploit_completes"`
	ExploreSkipRate float64 `json:"explore_skip_rate"`
	ExploitSkipRate float64 `json:"exploit_skip_rate"`
	ExploreCompRate float64 `json:"explore_complete_rate"`
	ExploitCompRate float64 `json:"exploit_complete_rate"`
	UniqueArtists7d int     `json:"unique_artists_7d"`
}

// DayPart is the exported name for API/queue blending.
func DayPart(h int) string { return dayPart(h) }

func dayPart(h int) string {
	switch {
	case h >= 5 && h < 12:
		return "morning"
	case h >= 12 && h < 17:
		return "afternoon"
	case h >= 17 && h < 23:
		return "evening"
	default:
		return "night"
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type PlaylistTrack struct {
	Position    int     `json:"position"`
	TrackID     int64   `json:"track_id"`
	Artist      string  `json:"artist"`
	Title       string  `json:"title"`
	Duration    float64 `json:"duration"`
	Explanation string  `json:"explanation"`
}

type Playlist struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	CreatedAt string          `json:"created_at"`
	Tracks    []PlaylistTrack `json:"tracks"`
}

type FavArtist struct {
	Artist   string `json:"artist"`
	Position int    `json:"position"`
	AddedAt  string `json:"added_at"`
}

type FavAlbum struct {
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Position int    `json:"position"`
	AddedAt  string `json:"added_at"`
}

type DiscoverTip struct {
	ID          int64   `json:"id"`
	Kind        string  `json:"kind"`
	Artist      string  `json:"artist"`
	Album       string  `json:"album"`
	Score       float64 `json:"score"`
	TrackIDs    []int64 `json:"track_ids"`
	Explanation string  `json:"explanation"`
	CreatedAt   string  `json:"created_at"`
}

type ArtistCount struct {
	Artist string `json:"artist"`
	Count  int    `json:"count"`
}

type ClusterCount struct {
	ClusterID int `json:"cluster_id"`
	Count     int `json:"count"`
}

type RadioShare struct {
	Token        string  `json:"token"`
	UserID       int64   // чей share (PG)
	Name         string  `json:"name"`
	CreatedAt    string  `json:"created_at"`
	RevokedAt    *string `json:"revoked_at,omitempty"`
	LastListenAt *string `json:"last_listen_at,omitempty"`
	ListenCount  int     `json:"listen_count"`
	Active       bool    `json:"active"`
}

type PlaySessionRow struct {
	ID           string
	UserID       int64 // чья сессия
	Mode         string
	CurrentID    int64
	QueueJSON    string
	ExcludeJSON  string
	RatedJSON    string
	DailyIDsJSON string
	DailyPos     int
	PlaylistName string
	PlaylistKind string
	UpdatedAt    string
}
