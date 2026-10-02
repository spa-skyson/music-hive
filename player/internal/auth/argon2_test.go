package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHashPasswordPHCFormat(t *testing.T) {
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=1,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", h)
	}
	h2, _ := HashPassword("pw")
	if h == h2 {
		t.Fatal("salts must differ between hashes")
	}
}

func TestVerifyPassword(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse", h) {
		t.Fatal("correct password must verify")
	}
	if VerifyPassword("battery staple", h) {
		t.Fatal("wrong password must not verify")
	}
	if VerifyPassword("x", "not-a-phc-string") {
		t.Fatal("malformed hash must not verify")
	}
	// хеш с нестандартными параметрами (будущие правки дефолтов) читается
	// по PHC-параметрам, а не по константам пакета
	custom := "$argon2id$v=19$m=1024,t=2,p=1$c29tZXNhbHRzb21lc2FsdA$" +
		"Im9GNHJhTHNDS05ZQ0R1dEdQcG5DQ3BxTjZTd1F1YkZ6cVZGMFZnPT0"
	if VerifyPassword("whatever", custom) {
		t.Fatal("fabricated custom-params hash must not verify")
	}
	if VerifyPassword("", "") {
		t.Fatal("empty hash must not verify")
	}
}

func TestSessionTokenRoundtrip(t *testing.T) {
	value, id, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 43 || strings.ContainsAny(value, "+/= ") {
		t.Fatalf("token value must be 43 url-safe chars: %q", value)
	}
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("id must be canonical uuid: %q", id)
	}
	got, ok := SessionID(value)
	if !ok || got != id {
		t.Fatalf("SessionID(%q)=%q,%v want %q", value, got, ok, id)
	}
	// производное, а не сырой токен: id не содержит value
	if strings.Contains(id, value) {
		t.Fatal("id must not embed the raw token")
	}
	for _, bad := range []string{"", "abc", "AAAA", strings.Repeat("A", 44), value + "x"} {
		if _, ok := SessionID(bad); ok {
			t.Fatalf("SessionID(%q) must reject", bad)
		}
	}
}

func TestAPIToken(t *testing.T) {
	tok, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, APITokenPrefix) || len(tok) != 4+43 {
		t.Fatalf("token shape: %q", tok)
	}
	h := HashAPIToken(tok)
	if len(h) != 64 {
		t.Fatalf("sha256-hex length = %d", len(h))
	}
	if HashAPIToken(tok) != h {
		t.Fatal("hash must be deterministic")
	}
	if HashAPIToken(tok+"x") == h {
		t.Fatal("different tokens must hash differently")
	}
}

func TestLoginLimiterAllowKey(t *testing.T) {
	l := NewLoginLimiter(3, time.Minute)
	req := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = "10.0.0.1:1"
		return r
	}
	for i := 0; i < 3; i++ {
		if !l.AllowKey(req(), "alice") {
			t.Fatalf("attempt %d for alice must pass", i)
		}
	}
	if l.AllowKey(req(), "alice") {
		t.Fatal("4th alice attempt must be limited")
	}
	// тот же IP, другой пользователь — не задет
	if !l.AllowKey(req(), "bob") {
		t.Fatal("bob from same IP must pass")
	}
	// другой IP, тот же пользователь — не задет
	other := httptest.NewRequest("POST", "/api/auth/login", nil)
	other.RemoteAddr = "10.0.0.2:1"
	if !l.AllowKey(other, "alice") {
		t.Fatal("alice from other IP must pass")
	}
	// ключ регистронезависим (citext)
	l2 := NewLoginLimiter(1, time.Minute)
	if !l2.AllowKey(req(), "Alice") {
		t.Fatal("first Alice attempt must pass")
	}
	if l2.AllowKey(req(), "alice") {
		t.Fatal("case-folded username must hit the same key")
	}
}
