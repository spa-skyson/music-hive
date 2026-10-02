package apitest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/static"
)

// TestPGSmoke: полный boot сервера в PG-режиме (MUSIC_HIVE_DATABASE_URL-ветка
// main.go) на self-seed данных: health, openapi, статус, similar и радио.
// Гейт: MUSIC_HIVE_TEST_DATABASE_URL, иначе skip (паттерн F1.1).
func TestPGSmoke(t *testing.T) {
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const dim = 4
	pgtest.ActivateModel(t, store.DB, "test:smoke", dim, true)
	ids := []int64{}
	for i := 0; i < 6; i++ {
		id := pgtest.InsertTrack(t, store.DB, "/smoke/"+string(rune('a'+i))+".flac",
			"Smoke Artist", "Smoke Album", "S"+string(rune('a'+i)))
		vec := make([]float32, dim)
		vec[i%dim] = 1
		if i >= dim {
			vec[(i+1)%dim] = 0.5
		}
		pgtest.InsertEmbedding(t, store.DB, "test:smoke", id, vec)
		ids = append(ids, id)
	}

	cfg := config.Config{
		DatabaseURL: dsn, QueueSize: 4, AuthDisabled: true,
		ProfileFormingAt: 3, ProfileReadyAt: 8, ExploreRatio: 0.15,
		WorkerAutostart: false, WorkerURL: "http://127.0.0.1:1",
	}
	idx := index.NewPG(cfg, store.DB)
	staticFS, err := static.Root()
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(cfg, store, idx, http.FS(staticFS))
	server.Play.Warm = nil
	if err := server.Reload(); err != nil {
		t.Fatalf("reload on PG: %v", err)
	}
	if idx.Size() != len(ids) {
		t.Fatalf("index size=%d want %d", idx.Size(), len(ids))
	}

	rec := serve(server, jsonReq("GET", "/api/health", ""))
	if rec.Code != 200 {
		t.Fatalf("health status=%d body=%s", rec.Code, rec.Body.String())
	}
	var health map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &health)
	if health["tracks"] != float64(len(ids)) {
		t.Fatalf("health tracks=%v want %d", health["tracks"], len(ids))
	}

	rec = serve(server, jsonReq("GET", "/api/openapi.json", ""))
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("openapi status=%d", rec.Code)
	}

	rec = serve(server, jsonReq("GET", "/api/status", ""))
	if rec.Code != 200 {
		t.Fatalf("status code=%d", rec.Code)
	}
	var status map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &status)
	if status["db"] != "postgres" {
		t.Fatalf("status db=%v want postgres", status["db"])
	}

	// similar: ближайший к орте (1,0,0,0) — сам трек id[0] исключён,
	// следующий — трек с компонентой в той же оси (i=4).
	rec = serve(server, jsonReq("GET", "/api/similar/"+strconv.FormatInt(ids[0], 10), ""))
	if rec.Code != 200 {
		t.Fatalf("similar status=%d body=%s", rec.Code, rec.Body.String())
	}

	// радио-старт: очередь собирается через CandidateRows/SimsFor по PG
	rec = serve(server, jsonReq("POST", "/api/radio/start", `{}`))
	if rec.Code != 200 {
		t.Fatalf("radio start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var radio struct {
		Queue []struct {
			TrackID int64 `json:"track_id"`
		} `json:"queue"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &radio)
	if len(radio.Queue) == 0 {
		t.Fatal("radio queue empty on PG")
	}
	flush(server)
}
