package db

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"runtime"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // драйвер "pgx" для database/sql
)

// PGStore — реализация Backend на PostgreSQL (фаза F1.3, GitLab #14).
// Схему НЕ создаёт: её применяет goose (`music-hive-player migrate`, F1.1).
//
// User-скопинг (F2.2): user-scoped методы Backend принимают userID явно;
// поле userID ниже — владелец для путей без запроса (boot: LoadReadyTracks
// и stats-join каталога, share-listen от имени владельца токена).
type PGStore struct {
	DB *sql.DB

	userID int64 // владелец для путей без аутентифицированного запроса

	modelKey     string // активная модель эмбеддингов (реестр embedding_models)
	modelDim     int
	embTable     string // физическая таблица векторов emb_<sanitize(model_key)>
	modelQueried bool
}

// OpenPG открывает пул к PostgreSQL, выставляет сессионные таймауты
// (statement/idle_in_transaction/lock) и разрешает пользователя-владельца.
func OpenPG(dsn string) (*PGStore, error) {
	dsn, err := dsnWithSessionOptions(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	maxConns := runtime.NumCPU() * 4
	if maxConns > 16 {
		maxConns = 16
	}
	if maxConns < 2 {
		maxConns = 2
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &PGStore{DB: db}
	if err := s.ensureOwnerUser(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("owner user: %w", err)
	}
	if err := s.ensureSchemaApplied(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *PGStore) Close() error { return s.DB.Close() }

// dsnWithSessionOptions добавляет сессионные таймауты через options,
// если оператор не задал их сам (см. design §5.1).
// Внимание: pgx разбирает query по RFC 3986 (свой uriDecode), где '+'
// не означает пробел — кодируем пробелы как %20.
func dsnWithSessionOptions(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	addParam := func(key, value string) {
		if strings.Contains(u.RawQuery, key+"=") {
			return
		}
		enc := strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += key + "=" + enc
	}
	addParam("options", "-c statement_timeout=10s -c idle_in_transaction_session_timeout=30s -c lock_timeout=5s")
	addParam("application_name", "player")
	return u.String(), nil
}

// ensureOwnerUser разрешает пользователя, от имени которого пишутся
// user-скопированные таблицы: is_owner, иначе минимальный id, иначе
// создаётся владелец 'owner' (пароль пуст — логин появится в F2,
// до этого auth работает от env, как раньше).
func (s *PGStore) ensureOwnerUser() error {
	return s.DB.QueryRow(`
WITH ins AS (
    INSERT INTO users (username, password_argon2, is_owner, is_admin)
    SELECT 'owner', '', TRUE, TRUE
    WHERE NOT EXISTS (SELECT 1 FROM users)
    RETURNING id
)
SELECT id FROM (
    SELECT id, TRUE AS pref FROM ins
    UNION ALL
    SELECT id, is_owner AS pref FROM users
) owners
ORDER BY pref DESC, id
LIMIT 1`).Scan(&s.userID)
}

// ensureSchemaApplied даёт понятную ошибку при старте на немигрированной БД.
func (s *PGStore) ensureSchemaApplied() error {
	var reg *string
	if err := s.DB.QueryRow(
		`SELECT to_regclass('public.embedding_models')`).Scan(&reg); err != nil {
		return err
	}
	if reg == nil {
		return fmt.Errorf("schema not applied: run `music-hive-player migrate up` (goose, F1.1)")
	}
	return nil
}

// ActiveModel возвращает (model_key, dim, имя emb-таблицы) активной модели
// из реестра embedding_models. Читается запросом, не кэшируется: модель
// может активировать worker (job model_activate) на живом сервере.
// Пустой model_key = модель не активирована (библиотека без векторов).
func (s *PGStore) ActiveModel() (key string, dim int, embTable string) {
	var k string
	var d int
	err := s.DB.QueryRow(
		`SELECT model_key, dim FROM embedding_models WHERE is_active`).Scan(&k, &d)
	if err == sql.ErrNoRows {
		return "", 0, ""
	}
	if err != nil {
		log.Printf("pg: embedding model registry: %v", err)
		return "", 0, ""
	}
	return k, d, EmbTableName(k)
}

// UserID — пользователь, от имени которого пишутся user-скопированные таблицы.
func (s *PGStore) UserID() int64 { return s.userID }

// EmbTableName — имя физической таблицы векторов для model_key
// ('clap:laion/...' → emb_clap_laion_...). Правило должно совпадать
// с sanitize у worker'а (F1.4): не-[a-z0-9] схлопываются в '_'.
func EmbTableName(modelKey string) string {
	var b strings.Builder
	b.WriteString("emb_")
	prevUnderscore := false
	for _, r := range strings.ToLower(modelKey) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		case !prevUnderscore:
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.TrimRight(b.String(), "_")
}

// pgNameNorm — нормформа имён артистов/альбомов для upsert-ключей
// (DDL: lower + trim + collapse whitespace).
func pgNameNorm(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// Int64Array рендерит литерал bigint[] ('{1,2}') для параметров вида
// ANY($n::bigint[]); пустой слайс → '{}'. Только int64 — инъекция невозможна.
// Общий хелпер db/index (vector-запросы живут в обоих пакетах).
func Int64Array(ids []int64) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	b.WriteByte('}')
	return b.String()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
