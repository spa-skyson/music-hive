package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPGOriginGuard — контракт issue #46: state-changing запросы с
// cookie-сессией принимаются только от доверенных origin'ов; Bearer —
// другая модель доверия и не проверяется; не-браузерные клиенты (без
// Origin/Sec-Fetch-Site) проходят.
func TestPGOriginGuard(t *testing.T) {
	server := openPGServer(t, "s3cret")

	// login → cookie
	login := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"s3cret"}`)
	login.RemoteAddr = "10.6.6.6:1"
	rec := serve(server, login)
	if rec.Code != 200 {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	sess := rec.Result().Cookies()
	if len(sess) != 1 {
		t.Fatalf("login cookies=%#v", sess)
	}

	// Bearer mht_ (создаётся same-origin POST'ом — случай same-origin заодно)
	tokReq := jsonReq("POST", "/api/auth/tokens", `{"name":"origin-guard"}`)
	tokReq.AddCookie(sess[0])
	tokReq.Host = "hive.local"
	tokReq.Header.Set("Origin", "http://hive.local")
	rec = serve(server, tokReq)
	if rec.Code != 200 {
		t.Fatalf("same-origin token create = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil || tok.Token == "" {
		t.Fatalf("token body: %v %s", err, rec.Body.String())
	}

	cookiePost := func(set func(*http.Request)) int {
		t.Helper()
		r := jsonReq("POST", "/api/auth/tokens", `{"name":"csrf-probe"}`)
		r.AddCookie(sess[0])
		r.Host = "hive.local"
		if set != nil {
			set(r)
		}
		return serve(server, r).Code
	}

	// чужой Origin → 403
	if c := cookiePost(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); c != 403 {
		t.Fatalf("evil Origin = %d, want 403", c)
	}
	// чужой Origin, гарантированно из браузера (Sec-Fetch-Site: same-site) → 403
	if c := cookiePost(func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "same-site")
	}); c != 403 {
		t.Fatalf("evil Origin same-site = %d, want 403", c)
	}
	// Sec-Fetch-Site: cross-site → 403, даже если Origin подменён на свой
	if c := cookiePost(func(r *http.Request) {
		r.Header.Set("Origin", "http://hive.local")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}); c != 403 {
		t.Fatalf("Sec-Fetch-Site cross-site = %d, want 403", c)
	}

	// same-origin и без Origin → 200; safe-метод guard не трогает
	if c := cookiePost(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }); c != 200 {
		t.Fatalf("same-origin = %d, want 200", c)
	}
	if c := cookiePost(nil); c != 200 {
		t.Fatalf("no Origin = %d, want 200 (не-браузерный клиент)", c)
	}
	get := jsonReq("GET", "/api/auth/tokens", "")
	get.AddCookie(sess[0])
	get.Host = "hive.local"
	get.Header.Set("Origin", "https://evil.example")
	if rec := serve(server, get); rec.Code != 200 {
		t.Fatalf("GET with evil Origin = %d, want 200 (guard только на unsafe)", rec.Code)
	}

	// CORS allow-list — доверенный cross-origin клиент проходит
	server.Cfg.CORSOrigins = []string{"https://friend.example"}
	if c := cookiePost(func(r *http.Request) { r.Header.Set("Origin", "https://friend.example") }); c != 200 {
		t.Fatalf("allow-list origin = %d, want 200", c)
	}
	server.Cfg.CORSOrigins = nil

	// Bearer — другая модель доверия: чужой Origin не отклоняется
	bearer := jsonReq("POST", "/api/auth/tokens", `{"name":"bearer-csrf"}`)
	bearer.Host = "hive.local"
	bearer.Header.Set("Authorization", "Bearer "+tok.Token)
	bearer.Header.Set("Origin", "https://evil.example")
	bearer.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := serve(server, bearer); rec.Code != 200 {
		t.Fatalf("Bearer with evil Origin = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// TestPGLoginContentType — анти login-CSRF (issue #46): логин принимается
// только с Content-Type: application/json (тип, недостижимый кросс-сайтовым
// simple-request'ом); text/plain/form-urlencoded/пустой → 415 до проверки
// пароля; суффикс charset допустим.
func TestPGLoginContentType(t *testing.T) {
	server := openPGServer(t, "s3cret")

	loginAs := func(contentType string) *httptest.ResponseRecorder {
		t.Helper()
		r := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"s3cret"}`)
		r.RemoteAddr = "10.4.4.4:1"
		if contentType == "" {
			r.Header.Del("Content-Type")
		} else {
			r.Header.Set("Content-Type", contentType)
		}
		return serve(server, r)
	}

	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/jsonX"} {
		rec := loginAs(ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("login Content-Type=%q = %d body=%s, want 415", ct, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "unsupported_media_type") {
			t.Fatalf("login Content-Type=%q: код ошибки без unsupported_media_type: %s", ct, rec.Body.String())
		}
	}
	// charset-суффикс — нормальный application/json
	if rec := loginAs("application/json; charset=utf-8"); rec.Code != 200 {
		t.Fatalf("login with charset = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	// корректный тип → прежнее поведение: неверный пароль по-прежнему 401
	wrong := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"nope"}`)
	wrong.RemoteAddr = "10.4.4.5:1"
	if rec := serve(server, wrong); rec.Code != 401 {
		t.Fatalf("wrong password = %d, want 401", rec.Code)
	}
}

// TestPGSecurityHeaders — CSP/nosniff/Referrer-Policy на статике и API.
func TestPGSecurityHeaders(t *testing.T) {
	server := openPGServer(t, "s3cret")
	for _, path := range []string{"/", "/api/health"} {
		rec := serve(server, jsonReq("GET", path, ""))
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		h := rec.Header()
		if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s CSP = %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q", path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("Referrer-Policy") == "" {
			t.Fatalf("%s: Referrer-Policy отсутствует", path)
		}
	}
}
