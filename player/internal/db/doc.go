// Package db — хранилище плеера на PostgreSQL (*PGStore, миграции goose).
//
//	pg.go        OpenPG, PGStore, сессионные таймауты
//	types.go     общие типы контракта Backend
//	backend.go   интерфейс Backend
//	pg_*.go      реализации по областям: каталог, история, джобы,
//	             плейлисты, профиль, радио, сессии, тексты, subsonic
package db
