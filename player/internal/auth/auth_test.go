package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsStaticAsset(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/assets/index-a1b2c3.js", true},
		{"/assets/logo-x1y2.svg", true},
		{"/assets/font-abc123.woff2", true},
		{"/foo.js", true},
		{"/foo.css", true},
		{"/favicon.ico", true},
		{"/api/assets/", false},
		{"/assets/../api/status", false},
		// Одинарное кодирование: net/http декодирует %2f в r.URL.Path
		// полностью, так что сюда приходит "/assets/../../x" — неканонический
		// путь, guard канонизации (path.Clean) его отсекает до whitelist.
		{"/assets/../../x", false},
		// Двойное кодирование: net/http декодирует %252f только один раз,
		// в r.URL.Path остаётся буквальное "%2f" как часть имени сегмента —
		// путь уже канонический и проходит whitelist по префиксу /assets/;
		// безопасность обеспечивает downstream http.FileServer+fs.Sub,
		// который резолвит "%2f" как литеральные символы имени файла и
		// не находит такой файл (404).
		{"/assets/..%2f..%2fx", true},
		{"//assets/x.js", false},
		{"/assets/./x.js", false},
		{"/", false},
		{"/api/status", false},
	} {
		if got := isStaticAsset(tc.path); got != tc.want {
			t.Errorf("isStaticAsset(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestMiddlewareBlocksAPIWithoutAuth — gate без хранилища (Users == nil)
// отвергает защищённые API внятным 401, не паникой; публичные пути
// (health) проходят.
func TestMiddlewareBlocksAPIWithoutAuth(t *testing.T) {
	gate := New(Config{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	h := gate.Middleware(next)

	req := httptest.NewRequest("GET", "/api/library", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d, want 401", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/health", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("public health status=%d", rec.Code)
	}

	// случайный Bearer — не per-user mht_ токен → 401
	req = httptest.NewRequest("GET", "/api/library", nil)
	req.Header.Set("Authorization", "Bearer some-random-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("random bearer status=%d, want 401", rec.Code)
	}
}

func TestLoginLimiterPerIPAndUser(t *testing.T) {
	lim := NewLoginLimiter(2, time.Minute)
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	req.RemoteAddr = "1.2.3.4:9"
	if !lim.AllowKey(req, "owner") || !lim.AllowKey(req, "owner") {
		t.Fatal("first two should allow")
	}
	if lim.AllowKey(req, "owner") {
		t.Fatal("third should block")
	}
	// соседний username с того же IP не задет
	if !lim.AllowKey(req, "other") {
		t.Fatal("other username should allow")
	}
	other := httptest.NewRequest("POST", "/api/auth/login", nil)
	other.RemoteAddr = "5.6.7.8:9"
	if !lim.AllowKey(other, "owner") {
		t.Fatal("other IP should allow")
	}
}

func TestEnabledFollowsDisabled(t *testing.T) {
	if !New(Config{}).Enabled() {
		t.Fatal("PG-auth enabled by default (owner guaranteed by bootstrap)")
	}
	if New(Config{Disabled: true}).Enabled() {
		t.Fatal("explicit disabled")
	}
}
