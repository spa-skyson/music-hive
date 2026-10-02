// Package migrations встраивает SQL-миграции goose (фаза F1, PostgreSQL+pgvector).
// Единственный источник схемы — эти миграции (отдельный DDL-файл удалён, #11).
package migrations

import "embed"

// FS содержит все *.sql-миграции; применяется через goose
// (player/internal/migrate, команда `music-hive-player migrate`).
//
//go:embed *.sql
var FS embed.FS
