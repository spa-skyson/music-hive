package db_test

import (
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/db"
)

// Живые PG-тесты auth.Store (F2.1): bootstrap владельца, сессии, токены.
// Гейт MUSIC_HIVE_TEST_DATABASE_URL, каждая тест-база свежая (pgtest.Open).

func TestPGAuthBootstrapOwnerUpsert(t *testing.T) {
	store := openPG(t) // OpenPG уже создал заглушку owner с пустым паролем

	// без пароля и без валидного владельца — fail-closed
	if err := auth.BootstrapOwner(store, ""); err != auth.ErrNoOwner {
		t.Fatalf("empty bootstrap = %v, want ErrNoOwner", err)
	}

	// bootstrap с паролем ОБНОВЛЯЕТ заглушку, не вставляет дубль
	if err := auth.BootstrapOwner(store, "s3cret"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var n int
	if err := store.DB.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users=%d err=%v, want 1 (no duplicate stub)", n, err)
	}
	creds, found, err := store.AuthLookupUser("owner")
	if err != nil || !found {
		t.Fatalf("lookup owner: found=%v err=%v", found, err)
	}
	if !auth.VerifyPassword("s3cret", creds.PasswordHash) {
		t.Fatal("bootstrapped hash must verify the env password")
	}
	if !creds.User.IsOwner || !creds.User.IsAdmin {
		t.Fatalf("owner flags: %+v", creds.User)
	}

	// повторный bootstrap с другим env-паролем НЕ перезаписывает БД
	if err := auth.BootstrapOwner(store, "other"); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	creds2, _, _ := store.AuthLookupUser("OWNER") // CITEXT — регистронезависимо
	if !auth.VerifyPassword("s3cret", creds2.PasswordHash) {
		t.Fatal("re-bootstrap must not silently rewrite the stored password")
	}

	// теперь без env-пароля валидный владелец есть — ошибки нет
	if err := auth.BootstrapOwner(store, ""); err != nil {
		t.Fatalf("empty bootstrap with valid owner: %v", err)
	}
}

func TestPGAuthBootstrapClaimsWorkerPlaceholder(t *testing.T) {
	store := openPG(t) // player-заглушка owner с пустым паролем

	// блокер F2: воркер стартует раньше player'а и успевает вставить
	// owner с $locked$-заглушкой (backend.owner_id)
	if _, err := store.DB.Exec(`UPDATE users SET password_argon2 = $1`,
		auth.WorkerPlaceholderPrefix+strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}

	// (в) без env-пароля заглушка — не пароль: объясняющая ошибка
	if err := auth.BootstrapOwner(store, ""); err != auth.ErrOwnerPlaceholder {
		t.Fatalf("empty bootstrap = %v, want ErrOwnerPlaceholder", err)
	}

	// (а) с env-паролем заглушка перезаписывается на месте: без дубля,
	// логин по env-паролю работает
	if err := auth.BootstrapOwner(store, "s3cret"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var n int
	if err := store.DB.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users=%d err=%v, want 1 (no duplicate stub)", n, err)
	}
	creds, found, err := store.AuthLookupUser("owner")
	if err != nil || !found {
		t.Fatalf("lookup owner: found=%v err=%v", found, err)
	}
	if !auth.VerifyPassword("s3cret", creds.PasswordHash) {
		t.Fatal("placeholder must be overwritten with the env password hash")
	}

	// (б) реальный пароль env-ом не перезаписывается (контракт F2.1)
	if err := auth.BootstrapOwner(store, "other"); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	creds2, _, _ := store.AuthLookupUser("owner")
	if !auth.VerifyPassword("s3cret", creds2.PasswordHash) {
		t.Fatal("real password must not be silently rewritten")
	}

	// после захвата без env-пароля валидный владелец есть — ошибки нет
	if err := auth.BootstrapOwner(store, ""); err != nil {
		t.Fatalf("empty bootstrap with valid owner: %v", err)
	}
}

func TestPGAuthSessionsAndTokens(t *testing.T) {
	store := openPG(t)
	if err := auth.BootstrapOwner(store, "s3cret"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	owner, found, err := store.AuthOwner()
	if err != nil || !found {
		t.Fatalf("AuthOwner: found=%v err=%v", found, err)
	}

	// сессия: создаётся, ищется по id-производному, удаляется
	_, id, err := auth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuthCreateSession(id, owner.ID, 14*24*time.Hour, "test-agent", "10.1.2.3"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, ok, err := store.AuthSessionUser(id)
	if err != nil || !ok || got.ID != owner.ID {
		t.Fatalf("session user: ok=%v user=%+v err=%v", ok, got, err)
	}
	// чужой/мусорный id не находится
	if _, ok, _ := store.AuthSessionUser("00000000-0000-0000-0000-000000000000"); ok {
		t.Fatal("unknown session id must not resolve")
	}
	if err := store.AuthDeleteSession(id); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, ok, _ := store.AuthSessionUser(id); ok {
		t.Fatal("deleted session must not resolve")
	}

	// токен: создаётся от имени пользователя, hash-lookup возвращает его
	tok, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuthCreateToken(owner.ID, "mobile", auth.HashAPIToken(tok), tok[:11]); err != nil {
		t.Fatalf("create token: %v", err)
	}
	tu, ok, err := store.AuthUserByTokenHash(auth.HashAPIToken(tok))
	if err != nil || !ok || tu.ID != owner.ID {
		t.Fatalf("token user: ok=%v user=%+v err=%v", ok, tu, err)
	}
	if _, ok, _ := store.AuthUserByTokenHash(auth.HashAPIToken("mht_forged")); ok {
		t.Fatal("forged token must not resolve")
	}

	// TTL сессии: истекшая не валидна, чистка удаляет
	_, id2, _ := auth.NewSessionToken()
	if err := store.AuthCreateSession(id2, owner.ID, -time.Second, "", ""); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, ok, _ := store.AuthSessionUser(id2); ok {
		t.Fatal("expired session must not resolve")
	}
	if n, err := store.AuthDeleteExpiredSessions(); err != nil || n != 1 {
		t.Fatalf("cleanup n=%d err=%v, want 1", n, err)
	}
	if n, _ := store.AuthDeleteExpiredSessions(); n != 0 {
		t.Fatalf("second cleanup n=%d, want 0", n)
	}
}

// TestPGAdminUsersAndTokens — F2.3 (GitLab #21) на уровне auth.Store:
// admin-CRUD, блокировка немедленно убивает сессии, список/отзыв токенов.
func TestPGAdminUsersAndTokens(t *testing.T) {
	store := openPG(t)
	if err := auth.BootstrapOwner(store, "s3cret"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// создать: хеш не течёт наружу, флаги по умолчанию
	hash, err := auth.HashPassword("bob-pass")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.AdminCreateUser("bob", hash, false)
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if bob.Username != "bob" || bob.IsAdmin || bob.IsOwner || bob.Disabled || bob.ID == 0 {
		t.Fatalf("bob=%+v", bob)
	}

	// дубликат имени — распознанная ошибка
	if _, err := store.AdminCreateUser("bob", hash, false); err != db.ErrUsernameTaken {
		t.Fatalf("duplicate = %v, want ErrUsernameTaken", err)
	}

	// список: владелец + bob, без хешей (тип AdminUser их не несёт)
	users, err := store.AdminListUsers()
	if err != nil || len(users) != 2 {
		t.Fatalf("list users=%d err=%v, want 2", len(users), err)
	}

	// сессия bob валидна; disable → немедленно мертва + revoked в БД
	_, sid, _ := auth.NewSessionToken()
	if err := store.AuthCreateSession(sid, bob.ID, time.Hour, "", ""); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, ok, _ := store.AuthSessionUser(sid); !ok {
		t.Fatal("session must resolve before disable")
	}
	dis := true
	if _, found, err := store.AdminUpdateUser(bob.ID, auth.AdminPatch{Disabled: &dis}); err != nil || !found {
		t.Fatalf("disable: found=%v err=%v", found, err)
	}
	if _, ok, _ := store.AuthSessionUser(sid); ok {
		t.Fatal("disabled user session must die immediately")
	}
	var revoked int
	if err := store.DB.QueryRow(
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NOT NULL`, bob.ID,
	).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("revoked rows=%d err=%v, want 1", revoked, err)
	}
	// disabled не входит и в логин-lookup как активный
	creds, found, err := store.AuthLookupUser("bob")
	if err != nil || !found || !creds.Disabled {
		t.Fatalf("lookup disabled bob: found=%v disabled=%v err=%v", found, creds.Disabled, err)
	}

	// токен disabled-пользователя тоже мёртв
	tok, _ := auth.NewAPIToken()
	if err := store.AuthCreateToken(bob.ID, "b", auth.HashAPIToken(tok), tok[:11]); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, ok, _ := store.AuthUserByTokenHash(auth.HashAPIToken(tok)); ok {
		t.Fatal("token of disabled user must not resolve")
	}

	// re-enable: логин-lookup снова активен; пароль не потерялся
	dis = false
	if _, _, err := store.AdminUpdateUser(bob.ID, auth.AdminPatch{Disabled: &dis}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	creds, _, _ = store.AuthLookupUser("bob")
	if creds.Disabled || !auth.VerifyPassword("bob-pass", creds.PasswordHash) {
		t.Fatalf("re-enabled bob creds: %+v", creds)
	}

	// смена пароля и админ-флаг одним патчем
	newHash, _ := auth.HashPassword("new-pass")
	admin := true
	got, found, err := store.AdminUpdateUser(bob.ID, auth.AdminPatch{PasswordHash: &newHash, IsAdmin: &admin})
	if err != nil || !found || !got.IsAdmin {
		t.Fatalf("patch bob: found=%v user=%+v err=%v", found, got, err)
	}
	creds, _, _ = store.AuthLookupUser("bob")
	if !auth.VerifyPassword("new-pass", creds.PasswordHash) || !creds.User.IsAdmin {
		t.Fatalf("patched bob creds: %+v", creds)
	}

	// несуществующий id
	if _, found, _ := store.AdminGetUser(1 << 40); found {
		t.Fatal("unknown user must not be found")
	}

	// токены: список с префиксом, отзыв — токен умирает
	tokens, err := store.AuthListTokens(bob.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("bob tokens=%d err=%v, want 1", len(tokens), err)
	}
	if tokens[0].Name != "b" || tokens[0].Prefix != tok[:11] {
		t.Fatalf("token info=%+v, prefix want %q", tokens[0], tok[:11])
	}
	if ok, err := store.AuthRevokeToken(bob.ID, tokens[0].ID); err != nil || !ok {
		t.Fatalf("revoke: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.AuthUserByTokenHash(auth.HashAPIToken(tok)); ok {
		t.Fatal("revoked token must not resolve")
	}
	// повторный отзыв / чужой id → false
	if ok, _ := store.AuthRevokeToken(bob.ID, tokens[0].ID); ok {
		t.Fatal("double revoke must report false")
	}
	if ok, _ := store.AuthRevokeToken(1<<40, tokens[0].ID); ok {
		t.Fatal("foreign revoke must report false")
	}
	// после отзыва список пуст
	if tokens, _ := store.AuthListTokens(bob.ID); len(tokens) != 0 {
		t.Fatalf("tokens after revoke=%d, want 0", len(tokens))
	}
}
