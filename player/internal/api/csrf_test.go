package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithSecurityHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := withSecurityHeaders(next)
	for _, path := range []string{"/", "/api/health", "/assets/x.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s CSP = %q", path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("%s Referrer-Policy = %q", path, got)
		}
	}
}

// TestWithCORSDropsWildcard: "*" не даёт cross-origin доступа даже при
// сборке Config мимо config.Validate (страховка; issue #46).
func TestWithCORSDropsWildcard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := withCORS(next, []string{"*"})
	// чужой origin — без ACAO/credentials
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("wildcard must not allow foreign origin")
	}
	// same-origin по-прежнему работает (allow-list опустел → same-origin ветка)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://example.com") // Host httptest по умолчанию
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("same-origin reflection must keep working")
	}
}

// TestMutationOriginTrusted — таблица решений origin-guard'а.
func TestMutationOriginTrusted(t *testing.T) {
	allow := map[string]bool{"friend.example": true}
	for _, tc := range []struct {
		name, origin, secFetchSite, host string
		want                             bool
	}{
		{"no headers (curl/воркер)", "", "", "hive.local", true},
		{"same-origin", "http://hive.local", "", "hive.local", true},
		{"evil origin", "https://evil.example", "", "hive.local", false},
		{"garbage origin", "not-a-url", "", "hive.local", false},
		{"cors allow-list", "https://friend.example", "", "hive.local", true},
		{"sfs cross-site wins", "http://hive.local", "cross-site", "hive.local", false},
		{"sfs cross-site без Origin", "", "cross-site", "hive.local", false},
		{"sfs same-origin ok", "http://hive.local", "same-origin", "hive.local", true},
		{"sfs same-site, но origin чужой", "https://other.example", "same-site", "hive.local", false},
		{"sfs none", "", "none", "hive.local", true},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/favorites", nil)
		req.Host = tc.host
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.secFetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
		}
		if got := mutationOriginTrusted(req, allow); got != tc.want {
			t.Errorf("%s: trusted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsUnsafeMethod — safe-методы и OPTIONS не проходят guard.
func TestIsUnsafeMethod(t *testing.T) {
	for m, want := range map[string]bool{
		http.MethodGet: false, http.MethodHead: false, http.MethodOptions: false,
		http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
		http.MethodDelete: true,
	} {
		if got := isUnsafeMethod(m); got != want {
			t.Errorf("isUnsafeMethod(%s) = %v, want %v", m, got, want)
		}
	}
}
