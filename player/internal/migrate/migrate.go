// Package migrate применяет goose-миграции (player/migrations) к PostgreSQL.
// Используется командой `music-hive-player migrate` (фаза F1, GitLab #12);
// сервер и воркер сами DDL не выполняют.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // регистрирует database/sql-драйвер "pgx"
	"github.com/pressly/goose/v3"

	"github.com/spa-skyson/music-hive/player/migrations"
)

func init() {
	goose.SetBaseFS(migrations.FS)
	// Статическая константа — паника здесь была бы ошибкой программирования.
	if err := goose.SetDialect("postgres"); err != nil {
		panic(err)
	}
	goose.SetLogger(stdoutLogger{log.New(os.Stdout, "", 0)})
}

// stdoutLogger направляет вывод goose (таблица status и пр.) в stdout,
// а не в stderr (дефолт goose).
type stdoutLogger struct{ l *log.Logger }

func (s stdoutLogger) Fatalf(format string, v ...any) { s.l.Printf(format, v...); os.Exit(1) }
func (s stdoutLogger) Printf(format string, v ...any) { s.l.Printf(format, v...) }

// CheckDSN валидирует DSN из MUSIC_HIVE_DATABASE_URL: непустой и postgres://
// (или postgresql://). Полный разбор делает pgx при подключении.
func CheckDSN(dsn string) error {
	if dsn == "" {
		return errors.New("MUSIC_HIVE_DATABASE_URL is not set (expected postgres:// DSN)")
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return fmt.Errorf("MUSIC_HIVE_DATABASE_URL: unexpected scheme, want postgres:// or postgresql://: %.32s", dsn)
	}
	return nil
}

func open(dsn string) (*sql.DB, error) {
	if err := CheckDSN(dsn); err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return db, nil
}

// Up применяет все неприменённые миграции (идемпотентно: на актуальной БД — no-op).
func Up(ctx context.Context, dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// Status печатает таблицу применённых/ожидающих миграций.
func Status(ctx context.Context, dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.StatusContext(ctx, db, "."); err != nil {
		return fmt.Errorf("goose status: %w", err)
	}
	return nil
}

// Down откатывает одну (последнюю применённую) миграцию — goose DownByOne.
func Down(ctx context.Context, dsn string) error {
	db, err := open(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := goose.DownContext(ctx, db, "."); err != nil {
		return fmt.Errorf("goose down: %w", err)
	}
	return nil
}

// Version возвращает текущую версию схемы БД (0 — миграций не применялось).
func Version(ctx context.Context, dsn string) (int64, error) {
	db, err := open(dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("goose version: %w", err)
	}
	return v, nil
}
