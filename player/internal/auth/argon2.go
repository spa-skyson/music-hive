package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const argon2Version = 0x13 // argon2id v19 (PHC "v=19")

type argon2Params struct {
	memoryKB int
	time     int
	threads  int
}

func argon2IDKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt,
		argon2Time, argon2MemoryKB, argon2Threads, argon2KeyLen)
}

func argon2IDKeyParams(password string, salt []byte, p argon2Params) []byte {
	return argon2.IDKey([]byte(password), salt,
		uint32(p.time), uint32(p.memoryKB), uint8(p.threads), argon2KeyLen)
}

// Argon2id-параметры (решение F2.1, GitLab #19): m=64 МБ, t=1, p=1, keyLen=32.
// 64 МБ памяти при одном проходе — выше OWASP-минимума «m=46 МиБ, t=1» по
// стойкости и заметно быстрее t=3 на слабом железе домашнего сервера;
// параметры вшиты в PHC-строку, поэтому verify следует за хранилищем.
const (
	argon2MemoryKB = 64 * 1024
	argon2Time     = 1
	argon2Threads  = 1
	argon2KeyLen   = 32
	argon2SaltLen  = 16
)

// ErrBadHash — повреждённая/нераспознанная PHC-строка в users.password_argon2.
var ErrBadHash = errors.New("auth: malformed argon2 PHC string")

// HashPassword возвращает PHC-строку
// $argon2id$v=19$m=65536,t=1,p=1$<salt>$<key> для пароля.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2IDKey(password, salt)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, argon2MemoryKB, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword сверяет пароль с PHC-строкой; сравнение constant-time.
func VerifyPassword(password, phc string) bool {
	params, salt, want, err := parsePHC(phc)
	if err != nil {
		return false
	}
	got := argon2IDKeyParams(password, salt, params)
	return hmac.Equal(got, want)
}

// dummyHash — хеш фиктивного пароля (default-параметры), вычисляется при
// инициализации пакета (~десятки мс, один раз): проверка логина для
// НЕсуществующего пользователя гоняет тот же argon2, чтобы время ответа
// не раскрывало, что имя занято (timing-атака на перечисление имён).
var dummyHash, _ = HashPassword("music-hive-dummy-password")

// VerifyDummy тратит время честной проверки пароля (для unknown user).
func VerifyDummy(password string) { _ = VerifyPassword(password, dummyHash) }

func parsePHC(phc string) (p argon2Params, salt, key []byte, err error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2Version {
		return p, nil, nil, ErrBadHash
	}
	for _, kv := range strings.Split(parts[3], ",") {
		switch {
		case strings.HasPrefix(kv, "m="):
			_, _ = fmt.Sscanf(kv, "m=%d", &p.memoryKB)
		case strings.HasPrefix(kv, "t="):
			_, _ = fmt.Sscanf(kv, "t=%d", &p.time)
		case strings.HasPrefix(kv, "p="):
			_, _ = fmt.Sscanf(kv, "p=%d", &p.threads)
		}
	}
	// guard: Sscanf %d пропускает отрицательные — как uint32 они стали бы
	// гигантскими значениями (m=2^31 КБ) и вешали бы verify
	if p.memoryKB <= 0 || p.time <= 0 || p.threads <= 0 {
		return p, nil, nil, ErrBadHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrBadHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrBadHash
	}
	return p, salt, key, nil
}

// ============================================================ токены

// NewSessionToken генерирует opaque-токен серверной сессии: 32 Б crypto/rand,
// cookie-значение — base64url. В БД лежит только производное: canonical UUID
// из первых 16 Б sha256(токена) (sessions.id — UUID PK по схеме 00001),
// поэтому утечка БД не даёт годных cookie.
func NewSessionToken() (value, id string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: session token: %w", err)
	}
	id, ok := sessionIDFromRaw(raw)
	if !ok {
		return "", "", errors.New("auth: session id derive")
	}
	return base64.RawURLEncoding.EncodeToString(raw), id, nil
}

// SessionID выводит id сессии из cookie-значения (см. NewSessionToken).
func SessionID(value string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", false
	}
	return sessionIDFromRaw(raw)
}

func sessionIDFromRaw(raw []byte) (string, bool) {
	if len(raw) != 32 {
		return "", false
	}
	sum := sha256.Sum256(raw)
	b := sum[:16]
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), true
}

// NewAPIToken генерирует per-user API-токен вида mht_<43 chars>
// (32 Б entropy). Секрет показывается один раз; в БД — sha256-hex.
func NewAPIToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: api token: %w", err)
	}
	return "mht_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashAPIToken — sha256-hex полного значения токена (с префиксом mht_).
func HashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// APITokenPrefix — маркер per-user Bearer-токенов F2.
const APITokenPrefix = "mht_"

// ============================================================ контекст

// User — аутентифицированный пользователь запроса (PG-режим, F2.1).
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	IsOwner  bool   `json:"is_owner"`
}

// Credentials — User + argon2-хеш (только для проверки логина; наружу
// не отдаётся).
type Credentials struct {
	User         User
	PasswordHash string
	Disabled     bool // F2.3: верный пароль, но аккаунт выключен → 403
}

type ctxKey struct{}

// WithUser кладёт пользователя запроса в context (мидлварь аутентификации).
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext достаёт пользователя, установленного мидлварью.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// Store — серверное хранилище пользователей/сессий/токенов (F2.1).
// Единственная реализация — *db.PGStore.
type Store interface {
	AuthLookupUser(username string) (Credentials, bool, error)
	AuthOwner() (User, bool, error)
	AuthOwnerHasPassword() (bool, error)
	AuthUpsertOwner(passwordHash string) error
	AuthCreateSession(id string, userID int64, ttl time.Duration, userAgent, ip string) error
	AuthSessionUser(id string) (User, bool, error)
	AuthDeleteSession(id string) error
	AuthDeleteExpiredSessions() (int64, error)
	AuthCreateToken(userID int64, name, tokenHash, tokenPrefix string) error
	AuthUserByTokenHash(tokenHash string) (User, bool, error)
	AuthListTokens(userID int64) ([]TokenInfo, error)
	AuthRevokeToken(userID, tokenID int64) (bool, error)

	// Управление пользователями (F2.3, admin-API). Пароли — только
	// argon2id-хеши; AdminUser наружу без хешей.
	AdminListUsers() ([]AdminUser, error)
	AdminGetUser(id int64) (AdminUser, bool, error)
	AdminCreateUser(username, passwordHash string, isAdmin bool) (AdminUser, error)
	AdminUpdateUser(id int64, p AdminPatch) (AdminUser, bool, error)
}

// AdminUser — строка списка пользователей (GET /api/admin/users): без
// хешей и прочих внутренностей.
type AdminUser struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	IsAdmin   bool      `json:"is_admin"`
	IsOwner   bool      `json:"is_owner"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
}

// AdminPatch — частичное обновление пользователя (PATCH /api/admin/users/{id});
// nil-поля не меняются.
type AdminPatch struct {
	Disabled     *bool
	PasswordHash *string
	IsAdmin      *bool
}

// TokenInfo — строка списка своих API-токенов (GET /api/auth/tokens):
// имя, префикс секрета (узнавание в менеджере паролей) и даты; сам секрет
// не хранится и не отдаётся.
type TokenInfo struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// WorkerPlaceholderPrefix — префикс пароля-заглушки, которую воркер
// (src/music_hive/db/backend.py, owner_id) вставляет в users('owner'),
// стартуя раньше player'а. Это не argon2-хеш — логин по нему невозможен,
// поэтому bootstrap перезаписывает заглушку из MUSIC_HIVE_PASSWORD, а
// fail-closed проверка считает её отсутствующим паролем.
const WorkerPlaceholderPrefix = "$locked$"

func isWorkerPlaceholder(phc string) bool {
	return strings.HasPrefix(phc, WorkerPlaceholderPrefix)
}

// ErrNoOwner — в PG нет пользователя-владельца с паролем (fail-closed).
var ErrNoOwner = errors.New("auth: owner has no password: set MUSIC_HIVE_PASSWORD and restart")

// ErrOwnerPlaceholder — владелец есть, но с заглушкой воркера, а
// MUSIC_HIVE_PASSWORD не задан: логин невозможен, пока заглушка не
// перезаписана env-паролём.
var ErrOwnerPlaceholder = errors.New("auth: owner exists with worker placeholder password; set MUSIC_HIVE_PASSWORD to claim it")

// BootstrapOwner применяет решение F2.1 «bootstrap owner»: с MUSIC_HIVE_PASSWORD
// создаёт/обновляет владельца (заглушки — пустая F1 и $locked$-заглушка
// воркера — обновляются на месте, без второй строки users), без пароля
// требует уже валидного владельца, иначе ErrNoOwner/ErrOwnerPlaceholder
// (старт запрещён).
func BootstrapOwner(st Store, password string) error {
	if password == "" {
		ok, err := st.AuthOwnerHasPassword()
		if err != nil {
			return err
		}
		if !ok {
			// владелец без пароля: различаем $locked$-заглушку воркера
			// и пустую заглушку F1.4 — сообщение подсказывает действие
			creds, found, err := st.AuthLookupUser("owner")
			if err != nil {
				return err
			}
			if found && isWorkerPlaceholder(creds.PasswordHash) {
				return ErrOwnerPlaceholder
			}
			return ErrNoOwner
		}
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return st.AuthUpsertOwner(hash)
}
