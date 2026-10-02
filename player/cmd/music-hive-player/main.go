package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/migrate"
	"github.com/spa-skyson/music-hive/player/internal/static"
)

func main() {
	// `music-hive-player migrate ...` — применение goose-миграций к PostgreSQL
	// (фаза F1). Перехват до config.Load: миграции не требуют auth/env старта.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(os.Args[2:])
		return
	}

	cfg := config.Load()
	// Fail-fast небезопасных комбинаций (issue #46): wildcard CORS +
	// credentials недопустим — проще не стартовать, чем молча отдавать
	// cookie-доступ любому origin.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	// PostgreSQL обязателен (#11): SQLite-стор удалён. Схему применяют
	// goose-миграции: `music-hive-player migrate up`.
	if cfg.DatabaseURL == "" {
		log.Fatal("требуется PostgreSQL: установите MUSIC_HIVE_DATABASE_URL (postgres://user:pass@host:5432/dbname?sslmode=disable); " +
			"схему примените командой `music-hive-player migrate up`. " +
			"Для переезда с SQLite см. migrate_sqlite.py и раздел DEPLOY.md про переезд")
	}
	// PG-аутентификация ничего из env не требует: bootstrap владельца
	// (MUSIC_HIVE_PASSWORD) опционален; MUSIC_HIVE_AUTH_DISABLED=1 —
	// локальный открытый режим.

	pg, err := db.OpenPG(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pg.Close()
	// Bootstrap владельца (F2.1): MUSIC_HIVE_PASSWORD → argon2id в
	// users (заглушка F1.4 обновляется на месте, без второй строки);
	// без env требуется уже валидный владелец, иначе fail-closed.
	if err := auth.BootstrapOwner(pg, cfg.Password); err != nil {
		if cfg.AuthDisabled {
			log.Printf("warning: owner bootstrap skipped (auth disabled): %v", err)
		} else {
			log.Fatalf("bootstrap owner: %v", err)
		}
	}
	idx := index.NewPG(cfg, pg.DB)
	log.Printf("music-hive-player db=postgres addr=%s", cfg.Addr)

	staticFS, err := static.Root()
	if err != nil {
		log.Fatalf("static: %v", err)
	}

	srv := api.New(cfg, pg, idx, http.FS(staticFS))
	if err := srv.Reload(); err != nil {
		log.Fatalf("reload: %v", err)
	}

	srv.EnsureWorker()
	srv.WatchJobs()
	srv.StartReloadProbe()

	if cfg.AuthEnabled() {
		log.Printf("auth enabled (owner bootstrap: MUSIC_HIVE_PASSWORD=%v)", cfg.Password != "")
	} else {
		log.Printf("auth explicitly disabled (MUSIC_HIVE_AUTH_DISABLED=1)")
	}
	addr := cfg.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}
	log.Printf("listening on http://%s  tracks=%d  sessions=multi", addr, idx.Size())
	if err := http.ListenAndServe(cfg.Addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

// runMigrate — CLI `music-hive-player migrate [up|status|down]`.
// DSN берётся из MUSIC_HIVE_DATABASE_URL (стандартный postgres://);
// без подкоманды выполняется up + status.
func runMigrate(args []string) {
	dsn := os.Getenv("MUSIC_HIVE_DATABASE_URL")
	if err := migrate.CheckDSN(dsn); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	var err error
	switch sub {
	case "up":
		if err = migrate.Up(ctx, dsn); err == nil {
			err = migrate.Status(ctx, dsn)
		}
	case "status":
		err = migrate.Status(ctx, dsn)
	case "down": // откат последней миграции
		err = migrate.Down(ctx, dsn)
	default:
		log.Fatalf("migrate: unknown subcommand %q (expected up|status|down)", sub)
	}
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
}
