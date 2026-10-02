package apitest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// openPGContractsServer — PG-сервер без предзагруженного индекса
// (для тестов, которым не нужны треки 11/22/33).
func openPGContractsServer(t *testing.T) (*api.Server, *db.PGStore, string) {
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
	return server, store, dir
}

func TestShareRadioCRUD(t *testing.T) {
	server, _, _ := openPGContractsServer(t)

	rec := serve(server, jsonReq(http.MethodPost, "/api/share/radio", `{"name":"Kitchen"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		Name  string `json:"name"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.OK || created.Token == "" || created.Name != "Kitchen" || created.URL == "" {
		t.Fatalf("created=%+v", created)
	}

	rec = serve(server, jsonReq(http.MethodGet, "/api/share/radio", ""))
	var active struct {
		Count  int `json:"count"`
		Shares []struct {
			Token  string `json:"token"`
			Active bool   `json:"active"`
		} `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Count != 1 || active.Shares[0].Token != created.Token || !active.Shares[0].Active {
		t.Fatalf("active shares=%+v", active)
	}

	rec = serve(server, jsonReq(http.MethodDelete, "/api/share/radio/"+created.Token, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serve(server, jsonReq(http.MethodGet, "/api/share/radio", ""))
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Count != 0 {
		t.Fatalf("active count after revoke=%d", active.Count)
	}

	rec = serve(server, jsonReq(http.MethodGet, "/api/share/radio?all=1", ""))
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Count != 1 || active.Shares[0].Active {
		t.Fatalf("all shares after revoke=%+v", active)
	}
}

func TestStreamOriginalAndErrors(t *testing.T) {
	server, store, dir := openPGContractsServer(t)
	audio := []byte("not-real-audio-but-served-verbatim")
	path := filepath.Join(dir, "sample.flac")
	if err := os.WriteFile(path, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(
		`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
		 VALUES (11,$1,'Track','Artist','Album',180)`, path,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(
		`SELECT setval(pg_get_serial_sequence('tracks','id'), (SELECT MAX(id) FROM tracks))`,
	); err != nil {
		t.Fatal(err)
	}

	rec := serve(server, jsonReq(http.MethodGet, "/api/stream/11", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); string(got) != string(audio) {
		t.Fatalf("stream body=%q, want %q", got, audio)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges=%q", got)
	}

	for _, tc := range []struct {
		path string
		want int
		code string
	}{
		{"/api/stream/999", http.StatusNotFound, "not_found"},
		{"/api/stream/not-an-id", http.StatusBadRequest, "bad_id"},
	} {
		rec = serve(server, jsonReq(http.MethodGet, tc.path, ""))
		if rec.Code != tc.want {
			t.Fatalf("%s status=%d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		var apiErr struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil {
			t.Fatal(err)
		}
		if apiErr.Code != tc.code {
			t.Fatalf("%s code=%q, want %q", tc.path, apiErr.Code, tc.code)
		}
	}
}

func TestReloadRequiresAuthOrLoopback(t *testing.T) {
	server, store, _ := openPGContractsServer(t)
	// включаем аутентификацию (fixture поднимает сервер с AUTH_DISABLED);
	// хранилище пользователей уже привязано к gate
	server.Auth.Cfg.Disabled = false

	req := jsonReq(http.MethodPost, "/api/reload", "")
	req.RemoteAddr = "203.0.113.10:1234"
	rec := serve(server, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote unauthenticated status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = jsonReq(http.MethodPost, "/api/reload", "")
	req.RemoteAddr = "127.0.0.1:1234"
	rec = serve(server, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback status=%d body=%s", rec.Code, rec.Body.String())
	}

	// per-user Bearer mht_ — валидная аутентификация и не с loopback
	userID := pgtest.InsertUser(t, store.DB, "reloader", "pw", false)
	tok, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuthCreateToken(userID, "reload", auth.HashAPIToken(tok), tok[:11]); err != nil {
		t.Fatal(err)
	}
	req = jsonReq(http.MethodPost, "/api/reload", "")
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = serve(server, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer status=%d body=%s", rec.Code, rec.Body.String())
	}
}
