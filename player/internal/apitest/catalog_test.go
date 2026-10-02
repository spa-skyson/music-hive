package apitest

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

func TestLibraryFiltersByArtistAndAlbum(t *testing.T) {
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, row := range []db.TrackRow{
		{ID: 1, Path: "/tmp/one.flac", Title: "One", Artist: "Massive Attack", Album: "Mezzanine", Duration: 180},
		{ID: 2, Path: "/tmp/two.flac", Title: "Two", Artist: "Massive Attack", Album: "Protection", Duration: 180},
		{ID: 3, Path: "/tmp/three.flac", Title: "Three", Artist: "Portishead", Album: "Dummy", Duration: 180},
	} {
		if _, err := store.DB.Exec(
			`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			row.ID, row.Path, row.Title, row.Artist, row.Album, row.Duration,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB.Exec(
		`SELECT setval(pg_get_serial_sequence('tracks','id'), (SELECT MAX(id) FROM tracks))`,
	); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		DatabaseURL: dsn, DBPath: filepath.Join(dir, "db", "t.db"),
		QueueSize: 6, AuthDisabled: true,
		ProfileFormingAt: 3, ProfileReadyAt: 8, ExploreRatio: 0.15,
		DiscoverExploreRatio: 0.35, WorkerURL: "http://127.0.0.1:1",
		WorkerAutostart: false,
	}
	idx := index.NewPG(cfg, store.DB)
	server := api.New(cfg, store, idx, nil)
	server.Play.Warm = nil

	tests := []struct {
		name  string
		query url.Values
		want  []int64
	}{
		{name: "all", want: []int64{1, 2, 3}},
		{name: "artist case insensitive", query: url.Values{"artist": {" massive attack "}}, want: []int64{1, 2}},
		{name: "artist and album", query: url.Values{"artist": {"Massive Attack"}, "album": {"mezzanine"}}, want: []int64{1}},
		{name: "unknown artist", query: url.Values{"artist": {"Unknown"}}, want: []int64{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := serve(server, jsonReq("GET", "/api/library?"+test.query.Encode(), ""))
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var rows []struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			got := make([]int64, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.ID)
			}
			if len(got) != len(test.want) {
				t.Fatalf("ids = %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("ids = %v, want %v", got, test.want)
				}
			}
		})
	}
}
