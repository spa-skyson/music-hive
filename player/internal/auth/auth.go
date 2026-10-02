package auth

import (
	"context"
	"crypto/sha256"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const CookieName = "music_hive_session"

type Config struct {
	TTL          time.Duration
	SecureCookie bool
	Disabled     bool
}

type Gate struct {
	Cfg Config

	// Users — серверное хранилище пользователей/сессий/токенов (F2.1);
	// заполняется при старте (PGStore реализует Store).
	Users Store

	ownerOnce sync.Once
	owner     User
	ownerOK   bool
}

func New(cfg Config) *Gate {
	if cfg.TTL <= 0 {
		cfg.TTL = 14 * 24 * time.Hour
	}
	return &Gate{Cfg: cfg}
}

// Enabled: аутентификация включена всегда (владелец гарантирован
// bootstrap'ом при старте); MUSIC_HIVE_AUTH_DISABLED=1 выключает.
func (g *Gate) Enabled() bool { return !g.Cfg.Disabled }

// DerivedSecret — детерминированный секрет из env-пароля, когда
// MUSIC_HIVE_SESSION_SECRET не задан (IKM для SubsonicCipher, F3.1).
func DerivedSecret(password string) []byte {
	h := sha256.Sum256([]byte("music-hive-session|" + password + "|"))
	return h[:]
}

// Middleware — аутентификация (F2.1): серверная сессия или Bearer-токен;
// пользователь кладётся в context (auth.FromContext).
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || publicPath(r) || isStaticAsset(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, via, ok := g.RequestUserVia(r)
		if !ok {
			// HTML navigations → still serve index (JS shows login); API/stream → 401
			if wantsJSON(r) || isProtectedMedia(r.URL.Path) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized","code":"auth_required"}`))
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithVia(WithUser(r.Context(), u), via)))
	})
}

// RequestUser решает, какой пользователь делает запрос:
// серверная сессия (cookie music_hive_session) → per-user Bearer mht_;
// MUSIC_HIVE_AUTH_DISABLED=1 → владелец (warning печатает main).
func (g *Gate) RequestUser(r *http.Request) (User, bool) {
	u, _, ok := g.RequestUserVia(r)
	return u, ok
}

// ViaSession/ViaToken/ViaOwner — способ аутентификации запроса (F2.1).
// Нужен origin-guard'у API (issue #46): CSRF-проверки на unsafe-методах
// применимы только к cookie-сессии — единственной учётной данных, которую
// браузер прикладывает к запросам автоматически. Bearer mht_ клиент хранит
// и прикладывает сам (другая модель доверия), ViaOwner — открытый режим
// без браузерных учётных данных вовсе.
type Via string

const (
	ViaSession Via = "session" // cookie music_hive_session
	ViaToken   Via = "token"   // Authorization: Bearer mht_…
	ViaOwner   Via = "owner"   // MUSIC_HIVE_AUTH_DISABLED=1
)

type viaKey struct{}

// WithVia кладёт способ аутентификации в context (рядом с WithUser).
func WithVia(ctx context.Context, via Via) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

// ViaFromContext достаёт способ аутентификации, установленный мидлварью.
func ViaFromContext(ctx context.Context) (Via, bool) {
	v, ok := ctx.Value(viaKey{}).(Via)
	return v, ok
}

// RequestUserVia — RequestUser + способ аутентификации.
func (g *Gate) RequestUserVia(r *http.Request) (User, Via, bool) {
	if g.Users == nil {
		return User{}, "", false
	}
	if g.Cfg.Disabled {
		u, ok := g.ownerUser()
		return u, ViaOwner, ok
	}
	if u, ok := g.sessionUser(r); ok {
		return u, ViaSession, true
	}
	if u, ok := g.tokenUser(r); ok {
		return u, ViaToken, true
	}
	return User{}, "", false
}

func (g *Gate) sessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	id, ok := SessionID(c.Value)
	if !ok {
		return User{}, false
	}
	u, found, err := g.Users.AuthSessionUser(id)
	if err != nil {
		log.Printf("auth: session lookup: %v", err)
		return User{}, false
	}
	return u, found
}

func (g *Gate) tokenUser(r *http.Request) (User, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return User{}, false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	// per-user токены F2: mht_<secret> → sha256-hex → api_tokens
	if strings.HasPrefix(tok, APITokenPrefix) {
		u, ok, err := g.Users.AuthUserByTokenHash(HashAPIToken(tok))
		if err == nil {
			return u, ok
		}
		log.Printf("auth: api token lookup: %v", err)
	}
	return User{}, false
}

// ownerUser — владелец для путей без собственных учётных данных
// (AUTH_DISABLED); резолвится один раз за жизнь процесса.
func (g *Gate) ownerUser() (User, bool) {
	g.ownerOnce.Do(func() {
		u, ok, err := g.Users.AuthOwner()
		if err != nil {
			log.Printf("auth: resolve owner: %v", err)
			ok = false
		}
		g.owner, g.ownerOK = u, ok
	})
	return g.owner, g.ownerOK
}

// SetSessionCookie пишет cookie серверной сессии: HttpOnly,
// SameSite=Lax, Path=/, Secure при MUSIC_HIVE_SECURE_COOKIE.
func (g *Gate) SetSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.Cfg.SecureCookie,
		MaxAge:   int(g.Cfg.TTL.Seconds()),
	})
}

// ClearCookie снимает сессионную cookie (logout).
func (g *Gate) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// Authenticated — запрос прошёл аутентификацию; для вспомогательных
// проверок вне мидлвари (health, /api/reload).
func (g *Gate) Authenticated(r *http.Request) bool {
	_, ok := g.RequestUser(r)
	return ok
}

func publicPath(r *http.Request) bool {
	p := r.URL.Path
	if r.Method == http.MethodPost && p == "/api/auth/login" {
		return true
	}
	// Worker reload callback: handler still enforces loopback or Bearer.
	if r.Method == http.MethodPost && p == "/api/reload" {
		return true
	}
	if r.Method == http.MethodGet {
		switch p {
		case "/api/health", "/api/openapi.json", "/api/auth/me", "/manifest.webmanifest":
			return true
		}
		// Share radio stream: token in path is the credential.
		if strings.HasPrefix(p, "/listen/") {
			return true
		}
	}
	// Subsonic /rest/* (F3.1): своя аутентификация в internal/subsonic
	// (u+t+s / u+p по спецификации), общий gate их не трогает.
	if strings.HasPrefix(p, "/rest/") || p == "/rest" {
		return true
	}
	return false
}

func isStaticAsset(p string) bool {
	// Мидлварь оборачивает mux снаружи (Server.Handler: withCORS(
	// withAPICacheControl(auth.Middleware(mux)))), так что на входе сюда
	// r.URL.Path ещё НЕ канонизирован ServeMux'ом (path.Clean происходит
	// внутри mux.ServeHTTP, уже после того как whitelist принял решение).
	// Поэтому канонизируем путь сами: если он неканонический (содержит
	// "..", "." или задвоенные "/" как сегменты), whitelist его в принципе
	// не рассматривает — ни по префиксу /assets/, ни по суффиксу файла.
	if c := path.Clean("/" + p); c != p {
		return false
	}
	// /assets/ — каталог собранных Vite-бандлов (JS/CSS/шрифты/изображения
	// с хешем в имени), в нём по определению нет приватных данных.
	if strings.HasPrefix(p, "/assets/") {
		return true
	}
	return p == "/favicon.ico" ||
		strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") ||
		strings.HasSuffix(p, ".webmanifest") || strings.HasSuffix(p, ".map")
}

func isProtectedMedia(p string) bool {
	return strings.HasPrefix(p, "/api/stream/") || strings.HasPrefix(p, "/api/artwork/") ||
		strings.HasPrefix(p, "/api/")
}

func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json")
}
