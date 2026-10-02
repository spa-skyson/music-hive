package api

import (
	"net/http"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// ============================================================ origin-guard
//
// Защита cookie-сессий от CSRF (issue #46). SameSite=Lax уже блокирует
// большинство cross-site POST'ов, но проверка Origin/Sec-Fetch-Site —
// эшелонированная защита: старые браузеры без SameSite, будущие обходы
// Lax, и явный отказ для cross-site независимо от эвристик браузера.
//
// Проверка применяется ТОЛЬКО к запросам, аутентифицированным cookie-
// сессией (auth.ViaSession): это единственная учётная дата, которую
// браузер прикладывает к запросам сам, — значит, единственная, которой
// злоумышленник может воспользоваться через браузер жертвы. Bearer mht_
// клиент хранит и прикладывает сам (cross-site JS до него не достанет),
// Subsonic /rest/* и loopback-воркер аутентифицируются своими схемами
// мимо общего gate, публичные пути (login, /listen/*) gate не проходят.
// Не-браузерные клиенты (curl, Subsonic-клиенты, воркер) не шлют
// Origin/Sec-Fetch-Site — и cookie у них нет, поэтому отсутствие обоих
// заголовков пропускается.

// isUnsafeMethod — методы, меняющие состояние (RFC 9110 safe methods).
func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// withOriginGuard оборачивает mux ВНУТРИ auth.Middleware (см. Handler):
// guard видит способ аутентификации в context.
func (s *Server) withOriginGuard(next http.Handler) http.Handler {
	// CORS allow-list в виде хостов origin'ов (MUSIC_HIVE_CORS_ORIGINS) —
	// доверенные cross-origin клиенты (SPA на соседнем origin) проходят.
	allow := map[string]bool{}
	for _, o := range s.Cfg.CORSOrigins {
		if o == "*" {
			continue // config.Validate фейлит старт; страховка от мимо-Load сборок
		}
		if h, err := parseOriginHost(o); err == nil {
			allow[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeMethod(r.Method) {
			if via, ok := auth.ViaFromContext(r.Context()); ok && via == auth.ViaSession {
				if !mutationOriginTrusted(r, allow) {
					writeErr(w, http.StatusForbidden, "csrf_origin",
						"cross-site запрос с cookie-сессией отклонён (Origin/Sec-Fetch-Site)")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// mutationOriginTrusted решает, может ли этот запрос с cookie-сессией
// менять состояние:
//   - Sec-Fetch-Site: cross-site (присылает только браузер, подделать со
//     страницы нельзя) → отказ сразу;
//   - иначе при наличии Origin хост Origin обязан совпадать с Host запроса
//     (same-origin) или входить в CORS allow-list;
//   - оба заголовка отсутствуют (curl, Subsonic-клиенты, воркер) → пропуск:
//     cookie у таких клиентов нет и не будет.
func mutationOriginTrusted(r *http.Request, allow map[string]bool) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host, err := parseOriginHost(origin)
	if err != nil {
		return false // мусорный Origin не считается доверенным
	}
	return host == r.Host || allow[host]
}

// ======================================================== security headers
//
// CSP и соседние заголовки на ВСЕ ответы (issue #46). CSP на JSON безвреден
// (браузер применяет его к документам и загрузке ресурсов). Политика строгая:
// фронт не имеет inline-скриптов/стилей (Vite-сборка, React-стили через CSSOM),
// внешних ресурсов нет (Google Fonts удалены), обложки/аудио — same-origin,
// аватар-fallback — data: (покрыт img-src data:).

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; media-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

// withSecurityHeaders — внешний слой Handler: заголовки попадают и на
// статику, и на API, и на preflight-ответы.
//
// Referrer-Policy: strict-origin-when-cross-origin (дефолт современных
// браузеров, здесь фиксируем явно для старых). Cross-origin получателю
// (открытие share-ссылки /listen/<token> из мессенджера) уходит только
// origin — путь с токеном не утекает; no-referrer строже, но и same-origin
// реферер для диагностики пропадает без выгоды: внешних ресурсов нет.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}
