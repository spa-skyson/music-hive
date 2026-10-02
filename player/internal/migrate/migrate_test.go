package migrate

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/migrations"
)

func TestCheckDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string // "" → без ошибки
	}{
		{name: "empty", dsn: "", want: "MUSIC_HIVE_DATABASE_URL is not set"},
		{name: "wrong scheme", dsn: "mysql://user:pass@host/db", want: "unexpected scheme"},
		{name: "postgres", dsn: "postgres://u:p@127.0.0.1:5432/db", want: ""},
		{name: "postgresql + sslmode", dsn: "postgresql://u:p@127.0.0.1:5432/db?sslmode=disable", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckDSN(tt.dsn)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckDSN(%q) = %v, want nil", tt.dsn, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CheckDSN(%q) = %v, want error containing %q", tt.dsn, err, tt.want)
			}
		})
	}
}

func TestUpBadDSNFailsFast(t *testing.T) {
	if err := Up(context.Background(), "sqlite3://nope"); err == nil {
		t.Fatal("Up with wrong scheme must fail, got nil")
	}
}

func TestMigrationsEmbedded(t *testing.T) {
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, "00001_init.sql") {
		t.Fatalf("00001_init.sql not embedded, got %v", files)
	}
	b, err := migrations.FS.ReadFile("00001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"-- +goose Up",
		"-- +goose Down",
		"CREATE EXTENSION IF NOT EXISTS vector",
		"CREATE EXTENSION IF NOT EXISTS citext",
		"CREATE TABLE users",
		"CREATE TABLE tracks",
		"CREATE TABLE embedding_models",
		"CREATE TABLE jobs",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("00001_init.sql: %q not found", want)
		}
	}
	if strings.Count(s, "-- +goose StatementBegin") != strings.Count(s, "-- +goose StatementEnd") {
		t.Error("unbalanced StatementBegin/StatementEnd")
	}
	// Таблицы emb_* создаёт активация модели (job model_activate), не миграция.
	if strings.Contains(s, "CREATE TABLE emb_") {
		t.Error("emb_* tables must not be created by migration")
	}
}

// TestMigrationsEmbedded002 — миграция F2.3: disabled у users и префикс
// API-токенов; goose-конвенция up/down (см. 00001, F1.1).
func TestMigrationsEmbedded002(t *testing.T) {
	b, err := migrations.FS.ReadFile("00002_users_disabled.sql")
	if err != nil {
		t.Fatalf("00002_users_disabled.sql not embedded: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		"-- +goose Up",
		"-- +goose Down",
		"ALTER TABLE users ADD COLUMN disabled BOOLEAN NOT NULL DEFAULT FALSE",
		"ALTER TABLE api_tokens ADD COLUMN token_prefix TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE users DROP COLUMN disabled",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("00002_users_disabled.sql: %q not found", want)
		}
	}
}

// TestMigrationsEmbedded003 — миграция F3.1: AES-GCM-blob subsonic-пароля
// (users.subsonic_password_enc); goose-конвенция up/down.
func TestMigrationsEmbedded003(t *testing.T) {
	b, err := migrations.FS.ReadFile("00003_subsonic_password.sql")
	if err != nil {
		t.Fatalf("00003_subsonic_password.sql not embedded: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		"-- +goose Up",
		"-- +goose Down",
		"ALTER TABLE users ADD COLUMN subsonic_password_enc TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE users DROP COLUMN subsonic_password_enc",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("00003_subsonic_password.sql: %q not found", want)
		}
	}
}

// allTables — все таблицы миграции 00001 (проверка полноты против живой БД).
var allTables = []string{
	"users", "sessions", "api_tokens",
	"artists", "albums", "tracks", "genres", "track_genres", "track_audio_features",
	"embedding_models", "user_taste_profiles", "user_feature_weights",
	"listening_history", "user_track_stats", "recommendation_impressions", "transitions",
	"playlists", "playlist_tracks", "user_favorites", "user_ratings", "user_listen_later",
	"play_sessions", "radio_shares",
	"lyrics", "discover_tips", "scan_state", "jobs",
}

// wantVersion — версия головной миграции (обновлять при добавлении новых).
const wantVersion = 3

// TestUpStatusDown проверяет против живого PostgreSQL: up → up (идемпотентность)
// → status → наличие таблиц/расширений → down → повторный up (БД остаётся мигрированной).
// Прогоняется только при заданном MUSIC_HIVE_TEST_DATABASE_URL, иначе skip:
//
//	MUSIC_HIVE_TEST_DATABASE_URL=postgres://music_hive:music_hive@127.0.0.1:5432/music_hive?sslmode=disable \
//	go test ./internal/migrate/ -run Live -v
func TestUpStatusDown(t *testing.T) {
	dsn := os.Getenv("MUSIC_HIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MUSIC_HIVE_TEST_DATABASE_URL not set; skipping live PG integration test")
	}
	ctx := context.Background()
	db := waitDB(t, dsn)
	defer db.Close()

	if err := Up(ctx, dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := Up(ctx, dsn); err != nil { // идемпотентность
		t.Fatalf("second up: %v", err)
	}
	if v, err := Version(ctx, dsn); err != nil || v != wantVersion {
		t.Fatalf("version = %d, %v; want %d", v, err, wantVersion)
	}
	if err := Status(ctx, dsn); err != nil {
		t.Fatalf("status: %v", err)
	}

	got := publicTables(t, db)
	for _, want := range allTables {
		if !slices.Contains(got, want) {
			t.Errorf("table %q missing after up (got %v)", want, got)
		}
	}
	for _, ext := range []string{"vector", "citext"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_extension WHERE extname = $1`, ext).Scan(&n); err != nil || n != 1 {
			t.Errorf("extension %q: n=%d err=%v, want 1", ext, n, err)
		}
	}

	// F2.3: колонки миграции 00002 на месте
	for _, col := range []struct{ table, name string }{
		{"users", "disabled"}, {"api_tokens", "token_prefix"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
			col.table, col.name).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s.%s: n=%d err=%v, want 1", col.table, col.name, n, err)
		}
	}

	// Down откатывает по одной миграции (DownByOne) — до нуля, все таблицы уходит
	for i := int64(0); i < wantVersion; i++ {
		if err := Down(ctx, dsn); err != nil {
			t.Fatalf("down #%d: %v", i+1, err)
		}
	}
	got = publicTables(t, db)
	for _, gone := range allTables {
		if slices.Contains(got, gone) {
			t.Errorf("table %q still exists after down", gone)
		}
	}

	if err := Up(ctx, dsn); err != nil { // вернуть БД в мигрированное состояние
		t.Fatalf("re-up after down: %v", err)
	}
}

func publicTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// waitDB ждёт готовности PG (healthcheck compose может ещё не пройти).
func waitDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.Ping(); err == nil {
			return db
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("postgres not ready in 30s")
	return nil
}
