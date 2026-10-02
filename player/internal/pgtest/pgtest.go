// Package pgtest — общая инфраструктура живых PG-тестов (паттерн F1.1):
// gated на env MUSIC_HIVE_TEST_DATABASE_URL, без него — t.Skip.
// Каждый Open создаёт отдельную базу (goose up) и дропает её по завершении,
// поэтому тесты не мешают ни друг другу, ни разделяемой music_hive.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/migrate"
)

// EnvDSN возвращает DSN для живых тестов или "" (вызывающий делает t.Skip).
func EnvDSN() string { return os.Getenv("MUSIC_HIVE_TEST_DATABASE_URL") }

// Open создаёт свежую мигрированную базу и возвращает её DSN.
func Open(t *testing.T) string {
	t.Helper()
	base := EnvDSN()
	if base == "" {
		t.Skip("MUSIC_HIVE_TEST_DATABASE_URL not set; skipping live PG integration test")
	}
	admin := WaitDB(t, base)
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("music_hive_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	if err := migrate.Up(context.Background(), dsn); err != nil {
		t.Fatalf("goose up on test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
	})
	return dsn
}

// WaitDB ждёт готовности PG (compose healthcheck может ещё не пройти).
func WaitDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	dbh, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := dbh.Ping(); err == nil {
			return dbh
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("postgres not ready in 30s")
	return nil
}

// ActivateModel повторяет job model_activate (F1.4) для тестов: создаёт
// физическую emb-таблицу активной модели и регистрирует её в реестре.
// withIndex — создавать ли HNSW (в юнит-тестах TopK на маленьких N он не
// нужен: планировщик и так делает seq scan, результат точный).
func ActivateModel(t *testing.T, dbh *sql.DB, modelKey string, dim int, withIndex bool) {
	t.Helper()
	table := db.EmbTableName(modelKey)
	if _, err := dbh.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    track_id    BIGINT PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    embedding   vector(%d) NOT NULL,
    status      TEXT NOT NULL DEFAULT 'ready',
    error       TEXT,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`, table, dim)); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	if withIndex {
		if _, err := dbh.Exec(fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s_hnsw ON %s USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`,
			table, table)); err != nil {
			t.Fatalf("create hnsw index: %v", err)
		}
	}
	if _, err := dbh.Exec(`
INSERT INTO embedding_models(model_key, backend, repo, dim, is_active, activated_at)
VALUES ($1, 'test', '', $2, TRUE, now())
ON CONFLICT (model_key) DO UPDATE SET is_active = TRUE, activated_at = now()`,
		modelKey, dim); err != nil {
		t.Fatalf("register model: %v", err)
	}
}

// InsertUser — пользователь с argon2-хешем пароля (изоляционные тесты F2.2;
// регистрация из UI — будущее F2.3, тестам она не нужна).
func InsertUser(t *testing.T, dbh *sql.DB, username, password string, isAdmin bool) int64 {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	var id int64
	if err := dbh.QueryRow(`
INSERT INTO users (username, password_argon2, is_admin)
VALUES ($1, $2, $3) RETURNING id`, username, hash, isAdmin).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

// InsertTrack — минимальный трек для тестов (IDENTITY сам выдаст id).
func InsertTrack(t *testing.T, dbh *sql.DB, path, artist, album, title string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`
INSERT INTO tracks(path, title, artist, album, duration, file_md5, created_at)
VALUES ($1,$2,$3,$4,$5,$6, now()) RETURNING id`,
		path, title, artist, album, 180.0, "md5-"+path).Scan(&id); err != nil {
		t.Fatalf("insert track %s: %v", path, err)
	}
	return id
}

// InsertEmbedding кладёт L2-нормированный вектор трека в emb-таблицу модели.
func InsertEmbedding(t *testing.T, dbh *sql.DB, modelKey string, trackID int64, vec []float32) {
	t.Helper()
	if _, err := dbh.Exec(
		`INSERT INTO `+db.EmbTableName(modelKey)+`(track_id, embedding, status) VALUES ($1,$2,'ready')`,
		trackID, pgvector.NewVector(vec)); err != nil {
		t.Fatalf("insert embedding track %d: %v", trackID, err)
	}
}
