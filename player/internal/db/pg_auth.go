package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// PGStore реализует auth.Store — серверные сессии, per-user API-токены и
// bootstrap владельца (F2.1, GitLab #19). Схема — 00001_init.sql
// (users/sessions/api_tokens), менять её этот код не может и не должен.

var _ auth.Store = (*PGStore)(nil)

// AuthLookupUser — по имени (CITEXT = регистронезависимо) для логина.
func (s *PGStore) AuthLookupUser(username string) (auth.Credentials, bool, error) {
	var c auth.Credentials
	err := s.DB.QueryRow(`
SELECT id, username, password_argon2, is_admin, is_owner, disabled
FROM users WHERE username = $1`, username).
		Scan(&c.User.ID, &c.User.Username, &c.PasswordHash, &c.User.IsAdmin, &c.User.IsOwner, &c.Disabled)
	if err == sql.ErrNoRows {
		return auth.Credentials{}, false, nil
	}
	if err != nil {
		return auth.Credentials{}, false, fmt.Errorf("pg: lookup user: %w", err)
	}
	return c, true, nil
}

// AuthOwner — пользователь-владелец (is_owner, иначе минимальный id —
// то же разрешение, что и ensureOwnerUser у OpenPG).
func (s *PGStore) AuthOwner() (auth.User, bool, error) {
	var u auth.User
	err := s.DB.QueryRow(`
SELECT id, username, is_admin, is_owner FROM users
ORDER BY is_owner DESC, id LIMIT 1`).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.IsOwner)
	if err == sql.ErrNoRows {
		return auth.User{}, false, nil
	}
	if err != nil {
		return auth.User{}, false, fmt.Errorf("pg: owner: %w", err)
	}
	return u, true, nil
}

// AuthOwnerHasPassword — есть ли владелец с непустым argon2-хешем
// (условие fail-closed старта без MUSIC_HIVE_PASSWORD). Заглушка воркера
// ($locked$…, см. auth.WorkerPlaceholderPrefix) паролем не считается:
// верификация по ней невозможна.
func (s *PGStore) AuthOwnerHasPassword() (bool, error) {
	var ok bool
	if err := s.DB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM users WHERE is_owner
		 AND password_argon2 <> '' AND password_argon2 NOT LIKE $1)`,
		auth.WorkerPlaceholderPrefix+"%",
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("pg: owner password check: %w", err)
	}
	return ok, nil
}

// AuthUpsertOwner — bootstrap владельца (решение F2.1): заглушки — пустая
// F1.4 и $locked$-заглушка воркера (воркер стартует раньше player'а) —
// ОБНОВЛЯЮТСЯ на месте; в пустой users вставляется владелец; существующий
// реальный пароль не трогается (env-пароль мог смениться, но argon2
// однонаправлен — перезапись env-ом была бы молчаливой сменой пароля,
// оставляем БД источником истины).
func (s *PGStore) AuthUpsertOwner(passwordHash string) error {
	res, err := s.DB.Exec(`
UPDATE users SET password_argon2 = $1
WHERE is_owner AND (password_argon2 IS NULL OR password_argon2 = ''
                    OR password_argon2 LIKE $2)`,
		passwordHash, auth.WorkerPlaceholderPrefix+"%")
	if err != nil {
		return fmt.Errorf("pg: upsert owner (update): %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	res, err = s.DB.Exec(`
INSERT INTO users (username, password_argon2, is_owner, is_admin)
SELECT 'owner', $1, TRUE, TRUE
WHERE NOT EXISTS (SELECT 1 FROM users)`, passwordHash)
	if err != nil {
		return fmt.Errorf("pg: upsert owner (insert): %w", err)
	}
	return nil
}

// AuthCreateSession — строка серверной сессии; id = canonical UUID из
// sha256(cookie-токена) (см. auth.NewSessionToken), сам токен не хранится.
func (s *PGStore) AuthCreateSession(id string, userID int64, ttl time.Duration, userAgent, ip string) error {
	var ipArg any
	if parsed := net.ParseIP(ip); parsed != nil {
		ipArg = parsed.String()
	}
	_, err := s.DB.Exec(`
INSERT INTO sessions (id, user_id, expires_at, user_agent, ip)
VALUES ($1::uuid, $2, now() + make_interval(secs => $3), $4, $5)`,
		id, userID, ttl.Seconds(), userAgent, ipArg)
	if err != nil {
		return fmt.Errorf("pg: create session: %w", err)
	}
	return nil
}

// AuthSessionUser — валидна ли сессия (не истекла, не отозвана) и чья.
// NOT u.disabled: выключенный пользователь теряет сессии немедленно (F2.3).
func (s *PGStore) AuthSessionUser(id string) (auth.User, bool, error) {
	var u auth.User
	err := s.DB.QueryRow(`
SELECT u.id, u.username, u.is_admin, u.is_owner
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.id = $1::uuid AND s.expires_at > now() AND s.revoked_at IS NULL
  AND NOT u.disabled`, id).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.IsOwner)
	if err == sql.ErrNoRows {
		return auth.User{}, false, nil
	}
	if err != nil {
		return auth.User{}, false, fmt.Errorf("pg: session user: %w", err)
	}
	return u, true, nil
}

func (s *PGStore) AuthDeleteSession(id string) error {
	if _, err := s.DB.Exec(`DELETE FROM sessions WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("pg: delete session: %w", err)
	}
	return nil
}

// AuthDeleteExpiredSessions — очистка просроченных сессий (вызывается при
// логине; idx_sessions_expires делает её дешёвой).
func (s *PGStore) AuthDeleteExpiredSessions() (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("pg: delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}

func (s *PGStore) AuthCreateToken(userID int64, name, tokenHash, tokenPrefix string) error {
	_, err := s.DB.Exec(
		`INSERT INTO api_tokens (user_id, name, token_hash, token_prefix) VALUES ($1, $2, $3, $4)`,
		userID, name, tokenHash, tokenPrefix)
	if err != nil {
		return fmt.Errorf("pg: create api token: %w", err)
	}
	return nil
}

// AuthUserByTokenHash — чей Bearer mht_-токен (sha256-hex); отзыв, срок и
// disabled пользователя (F2.3) учитываются.
func (s *PGStore) AuthUserByTokenHash(tokenHash string) (auth.User, bool, error) {
	var u auth.User
	err := s.DB.QueryRow(`
SELECT u.id, u.username, u.is_admin, u.is_owner
FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1 AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now())
  AND NOT u.disabled`, tokenHash).
		Scan(&u.ID, &u.Username, &u.IsAdmin, &u.IsOwner)
	if err == sql.ErrNoRows {
		return auth.User{}, false, nil
	}
	if err != nil {
		return auth.User{}, false, fmt.Errorf("pg: token user: %w", err)
	}
	return u, true, nil
}

// ============================================================ F2.3: admin

// ErrUsernameTaken — имя занято (users.username UNIQUE, CITEXT).
var ErrUsernameTaken = errors.New("pg: username already taken")

func scanAdminUser(row interface{ Scan(...any) error }) (auth.AdminUser, bool, error) {
	var u auth.AdminUser
	err := row.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.IsOwner, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.AdminUser{}, false, nil
	}
	if err != nil {
		return auth.AdminUser{}, false, fmt.Errorf("pg: scan admin user: %w", err)
	}
	return u, true, nil
}

// AdminListUsers — все пользователи без хешей (GET /api/admin/users).
func (s *PGStore) AdminListUsers() ([]auth.AdminUser, error) {
	rows, err := s.DB.Query(`
SELECT id, username, is_admin, is_owner, disabled, created_at
FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("pg: list users: %w", err)
	}
	defer rows.Close()
	out := []auth.AdminUser{}
	for rows.Next() {
		u, ok, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// AdminGetUser — один пользователь (PATCH-хендлеру для owner-защит).
func (s *PGStore) AdminGetUser(id int64) (auth.AdminUser, bool, error) {
	return scanAdminUser(s.DB.QueryRow(`
SELECT id, username, is_admin, is_owner, disabled, created_at
FROM users WHERE id = $1`, id))
}

// AdminCreateUser — argon2-хеш от хендлера; уникальность имени → ErrUsernameTaken.
func (s *PGStore) AdminCreateUser(username, passwordHash string, isAdmin bool) (auth.AdminUser, error) {
	u, ok, err := scanAdminUser(s.DB.QueryRow(`
INSERT INTO users (username, password_argon2, is_admin)
VALUES ($1, $2, $3)
RETURNING id, username, is_admin, is_owner, disabled, created_at`,
		username, passwordHash, isAdmin))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return auth.AdminUser{}, ErrUsernameTaken
		}
		return auth.AdminUser{}, fmt.Errorf("pg: create user: %w", err)
	}
	if !ok {
		return auth.AdminUser{}, errors.New("pg: create user: no row returned")
	}
	return u, nil
}

// AdminUpdateUser — частичное обновление (nil-поля не меняются); при
// disabled=true сессии пользователя отзываются сразу, чтобы повторное
// включение не оживило старые cookie. Owner-защита — в хендлере.
func (s *PGStore) AdminUpdateUser(id int64, p auth.AdminPatch) (auth.AdminUser, bool, error) {
	u, ok, err := scanAdminUser(s.DB.QueryRow(`
UPDATE users SET
  disabled        = COALESCE($2, disabled),
  password_argon2 = COALESCE($3, password_argon2),
  is_admin        = COALESCE($4, is_admin)
WHERE id = $1
RETURNING id, username, is_admin, is_owner, disabled, created_at`,
		id, p.Disabled, p.PasswordHash, p.IsAdmin))
	if err != nil || !ok {
		return auth.AdminUser{}, ok, err
	}
	if p.Disabled != nil && *p.Disabled {
		if _, err := s.DB.Exec(
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return auth.AdminUser{}, false, fmt.Errorf("pg: revoke user sessions: %w", err)
		}
	}
	return u, true, nil
}

// AuthListTokens — свои активные API-токены: имя, префикс, даты (F2.3).
func (s *PGStore) AuthListTokens(userID int64) ([]auth.TokenInfo, error) {
	rows, err := s.DB.Query(`
SELECT id, name, token_prefix, created_at, last_used_at
FROM api_tokens
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("pg: list api tokens: %w", err)
	}
	defer rows.Close()
	out := []auth.TokenInfo{}
	for rows.Next() {
		var ti auth.TokenInfo
		if err := rows.Scan(&ti.ID, &ti.Name, &ti.Prefix, &ti.CreatedAt, &ti.LastUsedAt); err != nil {
			return nil, fmt.Errorf("pg: scan api token: %w", err)
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// AuthRevokeToken — отзыв своего токена по id; false — нет такого
// (чужой/уже отозванный/не существует — не различаем).
func (s *PGStore) AuthRevokeToken(userID, tokenID int64) (bool, error) {
	res, err := s.DB.Exec(`
UPDATE api_tokens SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return false, fmt.Errorf("pg: revoke api token: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
