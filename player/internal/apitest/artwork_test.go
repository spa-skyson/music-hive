package apitest

import (
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// writeTestCover — png 120x80 во временный каталог.
func writeTestCover(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// openPGArtworkServer — PG-сервер с треками 11/22/33 (literal id)
// для byte-identical миграции artwork-тестов.
func openPGArtworkServer(t *testing.T) (*api.Server, *db.PGStore) {
	t.Helper()
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
	for _, id := range []int64{11, 22, 33} {
		path := filepath.Join(dir, "missing-"+itoa(id)+".flac")
		if _, err := store.DB.Exec(
			`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
			 VALUES ($1,$2,'Track','Artist','Album',180)`,
			id, path,
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
	return server, store
}

func TestHandleArtworkServesOriginalAndThumb(t *testing.T) {
	server, store := openPGArtworkServer(t)
	artPath := filepath.Join(t.TempDir(), "cover.png")
	writeTestCover(t, artPath)
	if _, err := store.DB.Exec(`UPDATE tracks SET artwork_path = $1 WHERE id = 11`, artPath); err != nil {
		t.Fatal(err)
	}

	rec := serve(server, httptest.NewRequest("GET", "/api/artwork/11", nil))
	if rec.Code != 200 {
		t.Fatalf("original status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type=%q, want image/png", ct)
	}
	if len(rec.Body.Bytes()) < 50 {
		t.Fatal("expected image bytes")
	}

	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/11?w=64", nil))
	if rec.Code != 200 {
		t.Fatalf("thumb status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("thumb content-type=%q, want image/jpeg", ct)
	}
	thumb := filepath.Join(filepath.Dir(filepath.Dir(server.Cfg.DBPath)), "cache", "art", "11_w64.jpg")
	if _, err := os.Stat(thumb); err != nil {
		t.Fatalf("thumb file missing: %v", err)
	}

	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/11?w=64", nil))
	if rec.Code != 200 {
		t.Fatalf("cached thumb status=%d", rec.Code)
	}

	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/22", nil))
	if rec.Code != 404 {
		t.Fatalf("missing artwork status=%d, want 404", rec.Code)
	}
}

func TestHandleArtworkBadID(t *testing.T) {
	server, _ := openPGArtworkServer(t)
	rec := serve(server, httptest.NewRequest("GET", "/api/artwork/x", nil))
	if rec.Code != 400 {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}
