package apitest

import (
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// TestPGArtworkWithoutEmbedding — PG-режим регресса прод-бага: обложка
// трека без эмбеддинга резолвится по каталогу (БД), индекс пуст.
// Гейт: MUSIC_HIVE_TEST_DATABASE_URL, иначе skip (паттерн F1.1).
func TestPGArtworkWithoutEmbedding(t *testing.T) {
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
	cover := filepath.Join(dir, "cover.png")
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{G: 120, A: 255})
		}
	}
	f, err := os.Create(cover)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// Трек с обложкой, но без эмбеддинга (фичи ещё не считались).
	withArt := pgtest.InsertTrack(t, store.DB, "/art/cover.flac", "Artist", "Album", "WithCover")
	if _, err := store.DB.Exec(`UPDATE tracks SET artwork_path = $1 WHERE id = $2`, cover, withArt); err != nil {
		t.Fatal(err)
	}
	// Трек без обложки (artwork_path NULL).
	noArt := pgtest.InsertTrack(t, store.DB, "/art/bare.flac", "Artist", "Album", "NoCover")

	cfg := config.Config{
		DatabaseURL: dsn, DBPath: filepath.Join(dir, "db", "t.db"),
		QueueSize: 4, AuthDisabled: true,
		ProfileFormingAt: 3, ProfileReadyAt: 8, ExploreRatio: 0.15,
		WorkerAutostart: false, WorkerURL: "http://127.0.0.1:1",
	}
	// Индекс нарочно не загружаем: /api/artwork не должен зависеть от него.
	idx := index.NewPG(cfg, store.DB)
	server := api.New(cfg, store, idx, nil)
	server.Play.Warm = nil

	rec := serve(server, httptest.NewRequest("GET",
		"/api/artwork/"+strconv.FormatInt(withArt, 10), nil))
	if rec.Code != 200 {
		t.Fatalf("no-embedding artwork status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type=%q, want image/png", ct)
	}

	// thumb-путь тоже работает (ServeArtworkFile + кэш)
	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/"+strconv.FormatInt(withArt, 10)+"?w=64", nil))
	if rec.Code != 200 {
		t.Fatalf("thumb status=%d body=%s", rec.Code, rec.Body.String())
	}

	// artwork_path = NULL → 404
	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/"+strconv.FormatInt(noArt, 10), nil))
	if rec.Code != 404 {
		t.Fatalf("null artwork status=%d, want 404", rec.Code)
	}

	// несуществующий id → 404
	rec = serve(server, httptest.NewRequest("GET", "/api/artwork/999999", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown id status=%d, want 404", rec.Code)
	}
}
