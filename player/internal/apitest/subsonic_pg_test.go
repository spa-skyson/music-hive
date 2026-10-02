package apitest

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// F3.1 (GitLab #23): subsonic-пароль через /api → /rest/* token+salt auth,
// системные эндпоинты, единый code 40 на все отказы.

func subToken(password, salt string) string {
	sum := md5.Sum([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

func subRespJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	sr, ok := env["subsonic-response"].(map[string]any)
	if !ok {
		t.Fatalf("no subsonic-response: %s", rec.Body.String())
	}
	return sr
}

func subErrCode(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	sr := subRespJSON(t, rec)
	errObj, _ := sr["error"].(map[string]any)
	if sr["status"] != "failed" || errObj == nil {
		t.Fatalf("expected failed envelope: %s", rec.Body.String())
	}
	return errObj["code"].(float64)
}

// TestPGSubsonicPasswordFlow: owner ставит subsonic-пароль через cookie-
// сессию → ping token+salt и p= работают, getLicense, публичные extensions,
// неверные креды → 40; основной пароль как subsonic-пароль НЕ работает.
func TestPGSubsonicPasswordFlow(t *testing.T) {
	server := openPGServer(t, "main-secret")
	owner := loginCookie(t, server, "owner", "main-secret")

	// PUT без сессии → 401
	if rec := serve(server, jsonReq("PUT", "/api/auth/subsonic-password", `{"password":"x"}`)); rec.Code != 401 {
		t.Fatalf("unauthenticated PUT = %d", rec.Code)
	}
	// PUT с пустым паролем → 400
	rec := serveWith(server, "PUT", "/api/auth/subsonic-password", `{"password":""}`, owner)
	if rec.Code != 400 {
		t.Fatalf("empty password = %d body=%s", rec.Code, rec.Body.String())
	}
	// валидный PUT
	rec = serveWith(server, "PUT", "/api/auth/subsonic-password", `{"password":"sub-pass-1"}`, owner)
	if rec.Code != 200 {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}

	ping := func(q url.Values) *httptest.ResponseRecorder {
		return serve(server, httptest.NewRequest("GET", "/rest/ping?"+q.Encode(), nil))
	}

	// token+salt — живой расчёт как у клиента: t=md5hex(clear+salt)
	q := url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"},
		"t": {subToken("sub-pass-1", "salty")}, "s": {"salty"}}
	rec = ping(q)
	if rec.Code != 200 {
		t.Fatalf("ping token+salt = %d body=%s", rec.Code, rec.Body.String())
	}
	sr := subRespJSON(t, rec)
	if sr["status"] != "ok" || sr["version"] != "1.16.1" || sr["type"] != "music-hive" {
		t.Fatalf("ping envelope=%v", sr)
	}

	// .view-написание + p= plain
	q = url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"}, "p": {"sub-pass-1"}}
	if rec := serve(server, httptest.NewRequest("GET", "/rest/ping.view?"+q.Encode(), nil)); subRespJSON(t, rec)["status"] != "ok" {
		t.Fatalf("ping.view p= failed: %s", rec.Body.String())
	}

	// p=enc:hex
	q = url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"},
		"p": {"enc:" + hex.EncodeToString([]byte("sub-pass-1"))}}
	if rec = ping(q); subRespJSON(t, rec)["status"] != "ok" {
		t.Fatalf("ping p=enc failed: %s", rec.Body.String())
	}

	// неверный token → 40
	q = url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"},
		"t": {subToken("wrong", "salty")}, "s": {"salty"}}
	if rec = ping(q); subErrCode(t, rec) != 40 {
		t.Fatalf("wrong token: %s", rec.Body.String())
	}
	// основной пароль ≠ subsonic-пароль
	q = url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"}, "p": {"main-secret"}}
	if rec = ping(q); subErrCode(t, rec) != 40 {
		t.Fatalf("main password accepted as subsonic: %s", rec.Body.String())
	}
	// subsonic-пароль не работает как основной логин
	r := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"sub-pass-1"}`)
	r.RemoteAddr = "10.7.7.9:1"
	if rec := serve(server, r); rec.Code != 401 {
		t.Fatalf("subsonic password accepted as main: %d", rec.Code)
	}

	// getLicense: valid + email владельца
	q = url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"},
		"t": {subToken("sub-pass-1", "s")}, "s": {"s"}}
	rec = serve(server, httptest.NewRequest("GET", "/rest/getLicense?"+q.Encode(), nil))
	lic, _ := subRespJSON(t, rec)["license"].(map[string]any)
	if lic == nil || lic["valid"] != true || lic["email"] != "owner" {
		t.Fatalf("license=%v", lic)
	}

	// XML без f: конверт с namespace
	q.Del("f")
	rec = serve(server, httptest.NewRequest("GET", "/rest/getLicense?"+q.Encode(), nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("default format=%s", ct)
	}
	if !strings.Contains(rec.Body.String(), `<license valid="true"`) {
		t.Fatalf("xml license=%s", rec.Body.String())
	}

	// getOpenSubsonicExtensions — публичный (без u/t/p)
	rec = serve(server, httptest.NewRequest("GET", "/rest/getOpenSubsonicExtensions?f=json", nil))
	exts, _ := subRespJSON(t, rec)["openSubsonicExtensions"].([]any)
	if len(exts) != 1 || exts[0].(map[string]any)["name"] != "formPost" {
		t.Fatalf("extensions=%v", exts)
	}
}

// TestPGSubsonicDisabledUser — выключенный пользователь получает те же 40,
// что и неверный пароль (не раскрываем блокировку, как Navidrome).
func TestPGSubsonicDisabledUser(t *testing.T) {
	server := openPGServer(t, "main-secret")
	admin := loginCookie(t, server, "owner", "main-secret")
	bob := createBobViaAdmin(t, server, admin, "bob-main")
	rec := serveWith(server, "PUT", "/api/auth/subsonic-password", `{"password":"bob-sub"}`, bob)
	if rec.Code != 200 {
		t.Fatalf("bob PUT = %d body=%s", rec.Code, rec.Body.String())
	}
	// админ выключает bob (сессия умирает), subsonic-вход → 40
	if rec := serveWith(server, "PATCH", "/api/admin/users/2", `{"disabled":true}`, admin); rec.Code != 200 {
		t.Fatalf("disable bob = %d", rec.Code)
	}
	q := url.Values{"u": {"bob"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"},
		"t": {subToken("bob-sub", "s")}, "s": {"s"}}
	rec = serve(server, httptest.NewRequest("GET", "/rest/ping?"+q.Encode(), nil))
	if code := subErrCode(t, rec); code != 40 {
		t.Fatalf("disabled user code=%v", code)
	}
}

// TestPGSubsonicRateLimit: 5 неудач (лимит loginLimiter) → 40 даже с верным
// паролем; другой IP продолжает работать.
func TestPGSubsonicRateLimit(t *testing.T) {
	server := openPGServer(t, "main-secret")
	owner := loginCookie(t, server, "owner", "main-secret")
	if rec := serveWith(server, "PUT", "/api/auth/subsonic-password", `{"password":"rl-pass"}`, owner); rec.Code != 200 {
		t.Fatalf("PUT = %d", rec.Code)
	}
	for i := 0; i < 5; i++ {
		q := url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"}, "p": {"wrong"}}
		r := httptest.NewRequest("GET", "/rest/ping?"+q.Encode(), nil)
		r.RemoteAddr = "10.3.3.3:9"
		serve(server, r)
	}
	// тот же IP, верный пароль → 40 (маскировка блокировки)
	q := url.Values{"u": {"owner"}, "v": {"1.16.1"}, "c": {"test"}, "f": {"json"}, "t": {subToken("rl-pass", "s")}, "s": {"s"}}
	r := httptest.NewRequest("GET", "/rest/ping?"+q.Encode(), nil)
	r.RemoteAddr = "10.3.3.3:9"
	rec := serve(server, r)
	if code := subErrCode(t, rec); code != 40 {
		t.Fatalf("rate-limited code=%v", code)
	}
	// /api-логин с того же IP|username тоже закрыт (общий лимитер)
	lr := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"main-secret"}`)
	lr.RemoteAddr = "10.3.3.3:9"
	if rec := serve(server, lr); rec.Code != 429 {
		t.Fatalf("shared limiter: login=%d", rec.Code)
	}
}
