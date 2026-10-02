package apitest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// openPGHTTPServer — PG-сервер с треками 11/22/33, активной моделью и
// загруженным индексом (server.Reload) для byte-identical миграции
// http-тестов, завязанных на Idx.Size/RowOf/MetaAt.
func openPGHTTPServer(t *testing.T) (*api.Server, *db.PGStore) {
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
	pgtest.ActivateModel(t, store.DB, "test:http", 2, false)
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
	for _, id := range []int64{11, 22, 33} {
		pgtest.InsertEmbedding(t, store.DB, "test:http", id, []float32{1, float32(id)})
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
	if err := server.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return server, store
}

func TestCatalogArtistsAlbumsTrack(t *testing.T) {
	server, _ := openPGHTTPServer(t)

	rec := serve(server, jsonReq("GET", "/api/artists", ""))
	var artists struct {
		Count   int `json:"count"`
		Artists []struct {
			Artist string `json:"artist"`
			Tracks int    `json:"tracks"`
		} `json:"artists"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &artists); err != nil {
		t.Fatal(err)
	}
	if artists.Count != 1 || artists.Artists[0].Tracks != 3 {
		t.Fatalf("artists=%+v", artists)
	}

	rec = serve(server, jsonReq("GET", "/api/albums", ""))
	var albums struct {
		Count  int `json:"count"`
		Albums []struct {
			Album string `json:"album"`
		} `json:"albums"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &albums); err != nil {
		t.Fatal(err)
	}
	if albums.Count != 1 || albums.Albums[0].Album != "Album" {
		t.Fatalf("albums=%+v", albums)
	}

	rec = serve(server, jsonReq("GET", "/api/tracks/11", ""))
	if rec.Code != 200 {
		t.Fatalf("track status=%d", rec.Code)
	}
	var track map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &track); err != nil {
		t.Fatal(err)
	}
	if track["id"].(float64) != 11 {
		t.Fatalf("track=%v", track)
	}

	rec = serve(server, jsonReq("GET", "/api/tracks/999", ""))
	if rec.Code != 404 {
		t.Fatalf("missing track status=%d", rec.Code)
	}
}

// TestPlayShuffleAndSessionShuffle: shuffle в /api/play и /api/session/shuffle.
func TestPlayShuffleAndSessionShuffle(t *testing.T) {
	server, store := openPGHTTPServer(t)

	// +20 треков того же артиста: на 23 треках случайное попадание
	// перемешанного трека на ожидаемую позицию практически исключено.
	for id := int64(1000); id < 1020; id++ {
		path := filepath.Join(t.TempDir(), "shuffle-"+itoa(id)+".flac")
		if _, err := store.DB.Exec(
			`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
			 VALUES ($1,$2,'Track','Artist','Album',180)`,
			id, path,
		); err != nil {
			t.Fatal(err)
		}
		pgtest.InsertEmbedding(t, store.DB, "test:http", id, []float32{1, float32(id)})
	}
	if _, err := store.DB.Exec(
		`SELECT setval(pg_get_serial_sequence('tracks','id'), (SELECT MAX(id) FROM tracks))`,
	); err != nil {
		t.Fatal(err)
	}
	if err := server.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// shuffle: true — стартовая позиция игнорируется, начинаем с начала
	// перемешанного списка.
	rec := serve(server, jsonReq("POST", "/api/play", `{"artist":"Artist","shuffle":true,"start_track_id":33}`))
	if rec.Code != 200 {
		t.Fatalf("play status=%d body=%s", rec.Code, rec.Body.String())
	}
	var play struct {
		SessionID string `json:"session_id"`
		Index     int    `json:"index"`
		Count     int    `json:"count"`
		Fixed     bool   `json:"fixed"`
		Current   struct {
			ID int64 `json:"id"`
		} `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &play); err != nil {
		t.Fatal(err)
	}
	if !play.Fixed || play.Count != 23 || play.Index != 0 {
		t.Fatalf("play=%+v", play)
	}

	// Перемешивание уже играющего плейлиста: текущий трек сохраняется,
	// count не меняется.
	rec = serve(server, jsonReq("POST", "/api/session/shuffle",
		fmt.Sprintf(`{"session_id":%q}`, play.SessionID)))
	if rec.Code != 200 {
		t.Fatalf("shuffle status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reshuffled struct {
		Index   int `json:"index"`
		Count   int `json:"count"`
		Current struct {
			ID int64 `json:"id"`
		} `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reshuffled); err != nil {
		t.Fatal(err)
	}
	if reshuffled.Count != 23 || reshuffled.Index != 0 || reshuffled.Current.ID != play.Current.ID {
		t.Fatalf("reshuffled=%+v, want count=23 index=0 current=%d", reshuffled, play.Current.ID)
	}

	// Радио-сессия не фиксированная — 4xx.
	rec = serve(server, jsonReq("POST", "/api/radio/start", ``))
	if rec.Code != 200 {
		t.Fatalf("radio status=%d body=%s", rec.Code, rec.Body.String())
	}
	var radio struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &radio); err != nil {
		t.Fatal(err)
	}
	rec = serve(server, jsonReq("POST", "/api/session/shuffle",
		fmt.Sprintf(`{"session_id":%q}`, radio.SessionID)))
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("radio shuffle status=%d body=%s, want 4xx", rec.Code, rec.Body.String())
	}
}

func TestPlayByArtistAndSessionJump(t *testing.T) {
	server, _ := openPGHTTPServer(t)

	rec := serve(server, jsonReq("POST", "/api/play", `{"artist":"Artist","start_track_id":33}`))
	if rec.Code != 200 {
		t.Fatalf("play status=%d body=%s", rec.Code, rec.Body.String())
	}
	var play struct {
		SessionID string `json:"session_id"`
		Index     int    `json:"index"`
		Count     int    `json:"count"`
		Fixed     bool   `json:"fixed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &play); err != nil {
		t.Fatal(err)
	}
	if !play.Fixed || play.Count != 3 || play.Index != 2 {
		t.Fatalf("play=%+v", play)
	}

	rec = serve(server, jsonReq("POST", "/api/session/jump",
		fmt.Sprintf(`{"session_id":%q,"track_id":11}`, play.SessionID)))
	if rec.Code != 200 {
		t.Fatalf("jump status=%d body=%s", rec.Code, rec.Body.String())
	}
	var jumped struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jumped); err != nil {
		t.Fatal(err)
	}
	if jumped.Index != 0 {
		t.Fatalf("jumped index=%d", jumped.Index)
	}
}

func TestLyricsAbsentAndPresent(t *testing.T) {
	server, store := openPGHTTPServer(t)

	rec := serve(server, jsonReq("GET", "/api/tracks/11/lyrics", ""))
	var absent map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &absent); err != nil {
		t.Fatal(err)
	}
	if absent["status"] != "absent" {
		t.Fatalf("absent=%v", absent)
	}

	if _, err := store.DB.Exec(`
INSERT INTO lyrics(track_id, plain_lyrics, synced_lyrics, source, source_id, instrumental, status, updated_at)
VALUES (11,'hello','','local','',FALSE,'ok',now())`); err != nil {
		t.Fatal(err)
	}
	rec = serve(server, jsonReq("GET", "/api/tracks/11/lyrics", ""))
	var ly struct {
		PlainLyrics string `json:"plain_lyrics"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ly); err != nil {
		t.Fatal(err)
	}
	if ly.PlainLyrics != "hello" || ly.Status != "ok" {
		t.Fatalf("lyrics=%+v", ly)
	}
}

func TestJobsLocalFallbackListGet(t *testing.T) {
	server, _ := openPGHTTPServer(t)

	rec := serve(server, jsonReq("POST", "/api/jobs/mix_pack", ""))
	if rec.Code != 200 {
		t.Fatalf("enqueue status=%d body=%s", rec.Code, rec.Body.String())
	}
	var enq struct {
		OK     bool   `json:"ok"`
		ID     int64  `json:"id"`
		JobID  int64  `json:"job_id"`
		Status string `json:"status"`
		Via    string `json:"via"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if !enq.OK || enq.ID == 0 || enq.JobID != enq.ID || enq.Via != "local_db" {
		t.Fatalf("enqueue=%+v", enq)
	}

	rec = serve(server, jsonReq("GET", "/api/jobs", ""))
	var listed struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count < 1 {
		t.Fatal("expected listed jobs")
	}

	rec = serve(server, jsonReq("GET", fmt.Sprintf("/api/jobs/%d", enq.ID), ""))
	if rec.Code != 200 {
		t.Fatalf("get job status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = serve(server, jsonReq("POST", "/api/library/rescan", ""))
	if rec.Code != 200 {
		t.Fatalf("rescan status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rescan struct {
		ID    int64 `json:"id"`
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rescan); err != nil {
		t.Fatal(err)
	}
	if rescan.ID == 0 || rescan.JobID != rescan.ID {
		t.Fatalf("rescan=%+v", rescan)
	}
}

func TestDiscoverTipsProfileHealthStatusMetrics(t *testing.T) {
	server, store := openPGHTTPServer(t)
	owner := store.UserID()
	if _, err := store.DB.Exec(`
INSERT INTO discover_tips(user_id, kind, artist, album, score, track_ids_json, explanation, created_at)
VALUES ($1,'new_album','Artist','Album',1.5,'[11,22]','fresh',now()),
       ($1,'resurfaced','Artist','Album',0.5,'[33]','old',now())`, owner,
	); err != nil {
		t.Fatal(err)
	}

	rec := serve(server, jsonReq("GET", "/api/discover/albums", ""))
	var tips struct {
		Tips []map[string]any `json:"tips"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tips); err != nil {
		t.Fatal(err)
	}
	if len(tips.Tips) != 1 {
		t.Fatalf("new album tips=%d", len(tips.Tips))
	}

	rec = serve(server, jsonReq("GET", "/api/discover/resurfaced", ""))
	if err := json.Unmarshal(rec.Body.Bytes(), &tips); err != nil {
		t.Fatal(err)
	}
	if len(tips.Tips) != 1 {
		t.Fatalf("resurfaced tips=%d", len(tips.Tips))
	}

	rec = serve(server, jsonReq("GET", "/api/profile", ""))
	if rec.Code != 200 {
		t.Fatalf("profile status=%d", rec.Code)
	}

	rec = serve(server, jsonReq("GET", "/api/health", ""))
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != true {
		t.Fatalf("health=%v", health)
	}

	rec = serve(server, jsonReq("GET", "/api/status", ""))
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["tracks"].(float64) != 3 {
		t.Fatalf("status tracks=%v", status["tracks"])
	}

	rec = serve(server, jsonReq("GET", "/api/metrics/weekly", ""))
	if rec.Code != 200 {
		t.Fatalf("weekly metrics status=%d", rec.Code)
	}

	rec = serve(server, jsonReq("GET", "/api/openapi.json", ""))
	if rec.Code != 200 || len(rec.Body.Bytes()) < 10 {
		t.Fatalf("openapi status=%d len=%d", rec.Code, rec.Body.Len())
	}

	rec = serve(server, jsonReq("GET", "/manifest.webmanifest", ""))
	if rec.Code != 200 || rec.Header().Get("Content-Type") == "" {
		t.Fatalf("manifest status=%d ct=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestRecommendFavoritesEmpty(t *testing.T) {
	server, _ := openPGHTTPServer(t)
	rec := serve(server, jsonReq("GET", "/api/recommend/favorites", ""))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["empty"] != true {
		t.Fatalf("expected empty favorites recommend: %#v", body)
	}
}

func TestRecommendHTTPErrors(t *testing.T) {
	server, _ := openPGHTTPServer(t)

	rec := serve(server, jsonReq("GET", "/api/similar/bad", ""))
	if rec.Code != 400 {
		t.Fatalf("bad similar id status=%d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("bad similar content-type=%q", got)
	}
	var apiErr struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Error != "bad id" || apiErr.Code != "bad_id" {
		t.Fatalf("bad similar error=%+v", apiErr)
	}

	rec = serve(server, jsonReq("GET", "/api/similar/artists", ""))
	if rec.Code != 400 {
		t.Fatalf("missing artist status=%d", rec.Code)
	}

	rec = serve(server, jsonReq("GET", "/api/recommend/seed", ""))
	if rec.Code != 400 {
		t.Fatalf("missing seed track status=%d", rec.Code)
	}
}

func TestRecommendationMetricsIncludesLatency(t *testing.T) {
	server, _ := openPGHTTPServer(t)
	rec := serve(server, jsonReq("GET", "/api/similar/11", ""))
	if rec.Code != 200 {
		t.Fatalf("similar status=%d", rec.Code)
	}

	rec = serve(server, jsonReq("GET", "/api/metrics/recommendations", ""))
	if rec.Code != 200 {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Window    string `json:"window"`
		LatencyMS map[string]struct {
			Count uint64 `json:"count"`
		} `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Window != "7d" {
		t.Fatalf("window=%q, want 7d", body.Window)
	}
	if body.LatencyMS["similar"].Count < 1 {
		t.Fatalf("expected similar latency samples, got %#v", body.LatencyMS)
	}
}
