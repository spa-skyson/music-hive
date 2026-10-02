package api

import (
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"strconv"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// handleAuthLogin: argon2id против users.password_argon2, серверная
// сессия в БД + cookie (F2.1). Username обязателен.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil || !s.Auth.Enabled() {
		writeJSON(w, map[string]any{"ok": true, "auth": false, "hint": "auth disabled"})
		return
	}
	s.pgLogin(w, r)
}

func (s *Server) pgLogin(w http.ResponseWriter, r *http.Request) {
	// Anti login-CSRF (issue #46): браузер не может отправить кросс-сайтовый
	// simple-request с Content-Type: application/json — этот тип требует
	// preflight, который без CORS-разрешения режется. HTML-форма атакующего
	// шлёт text/plain или x-www-form-urlencoded → 415 до каких-либо
	// проверок учётных данных (и без расхода rate-limit).
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"требуется Content-Type: application/json")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	if req.Username == "" {
		// логин без username запрещён: внятная ошибка, а не вход владельцем
		writeErr(w, 400, "username_required", "username is required")
		return
	}
	// rate-limit по (IP, username): брутфорс одного аккаунта блокируется,
	// не задевая соседей за тем же IP.
	if s.loginLimiter != nil && !s.loginLimiter.AllowKey(r, req.Username) {
		writeErr(w, 429, "rate_limited", "too many login attempts")
		return
	}
	creds, found, err := s.Auth.Users.AuthLookupUser(req.Username)
	if err != nil {
		writeErr(w, 500, "auth", err.Error())
		return
	}
	if !found || creds.PasswordHash == "" ||
		!auth.VerifyPassword(req.Password, creds.PasswordHash) {
		// неизвестное имя тоже гоняет argon2 — время ответа не выдаёт,
		// существует ли пользователь
		if !found || creds.PasswordHash == "" {
			auth.VerifyDummy(req.Password)
		}
		writeErr(w, 401, "invalid_credentials", "неверный логин или пароль")
		return
	}
	if creds.Disabled {
		// пароль верен — существование аккаунта уже не секрет; блокировка
		// отдельным кодом, чтобы выключенный пользователь понимал причину
		writeErr(w, 403, "account_disabled", "аккаунт отключён администратором")
		return
	}
	value, id, err := auth.NewSessionToken()
	if err != nil {
		writeErr(w, 500, "session", err.Error())
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if err := s.Auth.Users.AuthCreateSession(id, creds.User.ID, s.Auth.Cfg.TTL,
		r.UserAgent(), host); err != nil {
		writeErr(w, 500, "session", err.Error())
		return
	}
	// чистка просроченных сессий заодно с (редким) логином; неудача
	// не фатальна для входа
	_, _ = s.Auth.Users.AuthDeleteExpiredSessions()
	s.Auth.SetSessionCookie(w, value)
	writeJSON(w, map[string]any{"ok": true, "user": creds.User})
}

// handleAuthLogout: удаляет серверную сессию и чистит cookie.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		if c, err := r.Cookie(auth.CookieName); err == nil && c.Value != "" {
			if id, ok := auth.SessionID(c.Value); ok {
				if err := s.Auth.Users.AuthDeleteSession(id); err != nil {
					writeErr(w, 500, "session", err.Error())
					return
				}
			}
		}
		s.Auth.ClearCookie(w)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAuthMe: {ok, auth_enabled, id, username, is_admin, is_owner};
// без аутентификации ok:false.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	enabled := s.Auth != nil && s.Auth.Enabled()
	if s.Auth == nil {
		writeJSON(w, map[string]any{"ok": !enabled, "auth_enabled": enabled})
		return
	}
	u, ok := s.Auth.RequestUser(r)
	if !ok {
		u = auth.User{}
	}
	writeJSON(w, map[string]any{
		"ok": ok, "auth_enabled": enabled,
		"id": u.ID, "username": u.Username,
		"is_admin": u.IsAdmin, "is_owner": u.IsOwner,
	})
}

// handleAuthTokens: создать per-user API-токен (POST /api/auth/tokens) для
// себя. Секрет показывается один раз; префикс сохраняется для списка.
// GET — список своих токенов, DELETE /api/auth/tokens/{id} — отзыв (F2.3).
func (s *Server) handleAuthTokens(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authSelf(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // пустое тело допустимо
	}
	token, err := auth.NewAPIToken()
	if err != nil {
		writeErr(w, 500, "token", err.Error())
		return
	}
	prefix := token
	if len(prefix) > 11 { // mht_ + 7 символов — узнать в списке, не больше
		prefix = prefix[:11]
	}
	if err := s.Auth.Users.AuthCreateToken(u.ID, req.Name, auth.HashAPIToken(token), prefix); err != nil {
		writeErr(w, 500, "token", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "token": token, "name": req.Name})
}

// handleAuthTokensList: GET /api/auth/tokens — свои токены (имя, префикс,
// даты) без секретов.
func (s *Server) handleAuthTokensList(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authSelf(w, r)
	if !ok {
		return
	}
	tokens, err := s.Auth.Users.AuthListTokens(u.ID)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "tokens": tokens})
}

// handleAuthTokensRevoke: DELETE /api/auth/tokens/{id} — отзыв своего
// токена; чужой id неотличим от несуществующего → 404.
func (s *Server) handleAuthTokensRevoke(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authSelf(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad_id", "bad token id")
		return
	}
	revoked, err := s.Auth.Users.AuthRevokeToken(u.ID, id)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	if !revoked {
		writeErr(w, 404, "not_found", "token not found")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// authSelf — аутентифицированный пользователь «для себя»
// (AUTH_DISABLED резолвится во владельца). ok=false → ответ записан.
func (s *Server) authSelf(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := auth.FromContext(r.Context())
	if !ok {
		// собственная сессия: AUTH_DISABLED-режим тоже должен работать
		u, ok = s.Auth.RequestUser(r)
	}
	if !ok {
		writeErr(w, 401, "auth_required", "unauthorized")
		return auth.User{}, false
	}
	return u, true
}

// handleAuthSubsonicPassword — PUT /api/auth/subsonic-password (F3.1,
// GitLab #23): свой subsonic-пароль (отдельный от основного argon2id;
// основной не трогается). Хранение: users.subsonic_md5 = md5hex(clear) +
// users.subsonic_password_enc = AES-GCM(clear) для token+salt.
func (s *Server) handleAuthSubsonicPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authSelf(w, r)
	if !ok {
		return
	}
	ss, ok := s.Store.(subsonic.Store)
	if !ok {
		// недостижимо с PG-стором (реализует subsonic.Store), защита от
		// будущих реализаций Backend без subsonic
		writeErr(w, 501, "not_supported",
			"subsonic requires PostgreSQL (MUSIC_HIVE_DATABASE_URL)")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	// Граница доверия: 1..256 символов. Пустой пароль запрещён — иначе
	// блокировка subsonic-входа была бы случайной (job'ы PUT без тела).
	if n := len(req.Password); n < 1 || n > 256 {
		writeErr(w, 400, "bad_password", "password must be 1..256 characters")
		return
	}
	cipher := s.subsonic.Cipher
	if cipher == nil {
		writeErr(w, 500, "cipher", "subsonic cipher unavailable (check MUSIC_HIVE_SUBSONIC_SECRET)")
		return
	}
	enc, err := cipher.Seal(req.Password)
	if err != nil {
		writeErr(w, 500, "cipher", err.Error())
		return
	}
	if err := ss.AuthSetSubsonicPassword(u.ID, auth.MD5Hex(req.Password), enc); err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "username": u.Username})
}
