package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/static"
)

// openPGServer поднимает сервер в PG-режиме с бутстрапнутым владельцем
// (main.go-путь F2.1: OpenPG → BootstrapOwner → api.New). Необязательный
// workerURL подменяет недоступный по умолчанию воркер (моки F4.3).
func openPGServer(t *testing.T, password string, workerURL ...string) *api.Server {
	t.Helper()
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := auth.BootstrapOwner(store, password); err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	worker := "http://127.0.0.1:1"
	if len(workerURL) > 0 {
		worker = workerURL[0]
	}
	cfg := config.Config{
		DatabaseURL: dsn, QueueSize: 4, Password: password,
		ProfileFormingAt: 3, ProfileReadyAt: 8, ExploreRatio: 0.15,
		WorkerAutostart: false, WorkerURL: worker,
	}
	idx := index.NewPG(cfg, store.DB) // каталог пуст (без Load индекс инертен); для auth-тестов хватает
	staticFS, err := static.Root()
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(cfg, store, idx, http.FS(staticFS))
	server.Play.Warm = nil
	return server
}

// TestPGAuthFailClosed: без MUSIC_HIVE_PASSWORD и без валидного владельца
// bootstrap запрещает старт (GitLab #19, fail-closed).
func TestPGAuthFailClosed(t *testing.T) {
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := auth.BootstrapOwner(store, ""); err != auth.ErrNoOwner {
		t.Fatalf("bootstrap without password = %v, want ErrNoOwner", err)
	}
}

// TestPGAuthFlow: login (owner по явному username) → me →
// API-токен → Bearer → logout → сессия мертва. Плюс cookie-флаги,
// отказ без username/чужому Bearer и rate-limit.
func TestPGAuthFlow(t *testing.T) {
	server := openPGServer(t, "s3cret")

	// неверный пароль — единый 401 (без раскрытия, существует ли имя)
	bad := jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"nope"}`)
	bad.RemoteAddr = "10.7.7.7:1"
	if rec := serve(server, bad); rec.Code != 401 {
		t.Fatalf("bad password status=%d body=%s", rec.Code, rec.Body.String())
	}
	ghost := jsonReq("POST", "/api/auth/login", `{"username":"ghost","password":"nope"}`)
	ghost.RemoteAddr = "10.7.7.8:1"
	if rec := serve(server, ghost); rec.Code != 401 {
		t.Fatalf("unknown user status=%d, want same 401", rec.Code)
	}

	// без username → 400 (легаси-путь «пароль без имени» удалён)
	rec := serve(server, jsonReq("POST", "/api/auth/login", `{"password":"s3cret"}`))
	if rec.Code != 400 {
		t.Fatalf("login without username status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}

	// явный username → owner
	rec = serve(server, jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"s3cret"}`))
	if rec.Code != 200 {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var login struct {
		OK   bool `json:"ok"`
		User struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			IsOwner  bool   `json:"is_owner"`
			IsAdmin  bool   `json:"is_admin"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if !login.OK || login.User.Username != "owner" || !login.User.IsOwner || !login.User.IsAdmin || login.User.ID == 0 {
		t.Fatalf("login user=%+v", login.User)
	}

	// cookie-флаги: HttpOnly, SameSite=Lax, Path=/
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.CookieName {
		t.Fatalf("cookies=%#v", cookies)
	}
	sess := cookies[0]
	if !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.Value == "" {
		t.Fatalf("cookie flags: httponly=%v samesite=%v path=%q", sess.HttpOnly, sess.SameSite, sess.Path)
	}

	meWith := func(r *http.Request) (int, map[string]any) {
		rec := serve(server, r)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	// me по cookie
	req := jsonReq("GET", "/api/auth/me", "")
	req.AddCookie(sess)
	code, me := meWith(req)
	if code != 200 || me["ok"] != true || me["auth_enabled"] != true || me["username"] != "owner" {
		t.Fatalf("me(cookie)=%d %v", code, me)
	}

	// me без аутентификации — публичный, ok:false
	_, me = meWith(jsonReq("GET", "/api/auth/me", ""))
	if me["ok"] != false || me["auth_enabled"] != true {
		t.Fatalf("me(anon)=%v", me)
	}

	// защищённый эндпоинт: без auth 401, с cookie 200
	if rec := serve(server, jsonReq("GET", "/api/status", "")); rec.Code != 401 {
		t.Fatalf("status without auth = %d, want 401", rec.Code)
	}
	st := jsonReq("GET", "/api/status", "")
	st.AddCookie(sess)
	if rec := serve(server, st); rec.Code != 200 {
		t.Fatalf("status with cookie = %d", rec.Code)
	}

	// легаси env-токен удалён: произвольный Bearer не аутентифицирует
	legacy := jsonReq("GET", "/api/auth/me", "")
	legacy.Header.Set("Authorization", "Bearer legacy-env-token")
	code, me = meWith(legacy)
	if code != 200 || me["ok"] != false {
		t.Fatalf("me(unknown bearer)=%d %v, want ok:false", code, me)
	}

	// свой API-токен: создаётся, работает как Bearer mht_
	tokReq := jsonReq("POST", "/api/auth/tokens", `{"name":"pytest"}`)
	tokReq.AddCookie(sess)
	rec = serve(server, tokReq)
	if rec.Code != 200 {
		t.Fatalf("create token status=%d body=%s", rec.Code, rec.Body.String())
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil || len(tok.Token) < 40 {
		t.Fatalf("token response: %v %q", err, tok.Token)
	}
	bearer := jsonReq("GET", "/api/auth/me", "")
	bearer.Header.Set("Authorization", "Bearer "+tok.Token)
	code, me = meWith(bearer)
	if code != 200 || me["ok"] != true || me["username"] != "owner" {
		t.Fatalf("me(mht bearer)=%d %v", code, me)
	}
	// подделка не проходит
	forged := jsonReq("GET", "/api/auth/me", "")
	forged.Header.Set("Authorization", "Bearer mht_forgedtoken")
	if _, me := meWith(forged); me["ok"] == true {
		t.Fatal("forged token must not authenticate")
	}

	// logout: сессия удаляется, cookie чистится
	out := jsonReq("POST", "/api/auth/logout", "")
	out.AddCookie(sess)
	outRec := serve(server, out)
	if outRec.Code != 200 {
		t.Fatalf("logout status=%d", outRec.Code)
	}
	if c := outRec.Result().Cookies(); len(c) != 1 || c[0].Value != "" || c[0].MaxAge != -1 {
		t.Fatalf("logout must clear cookie, got %#v", c)
	}
	dead := jsonReq("GET", "/api/status", "")
	dead.AddCookie(sess)
	if rec := serve(server, dead); rec.Code != 401 {
		t.Fatalf("status after logout = %d, want 401", rec.Code)
	}
}

// TestPGAuthSecureCookie: MUSIC_HIVE_SECURE_COOKIE → Secure-флаг.
func TestPGAuthSecureCookie(t *testing.T) {
	server := openPGServer(t, "s3cret")
	server.Auth.Cfg.SecureCookie = true
	rec := serve(server, jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"s3cret"}`))
	if rec.Code != 200 {
		t.Fatalf("login status=%d", rec.Code)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || !c[0].Secure {
		t.Fatalf("cookie must carry Secure flag: %#v", c)
	}
}

// TestPGAuthRateLimit: (IP, username) — 5 попыток → 429; соседний
// пользователь с того же IP не задет.
func TestPGAuthRateLimit(t *testing.T) {
	server := openPGServer(t, "s3cret")
	attempt := func(user, ip string) int {
		r := jsonReq("POST", "/api/auth/login", `{"username":"`+user+`","password":"bad"}`)
		r.RemoteAddr = ip + ":1234"
		return serve(server, r).Code
	}
	for i := 0; i < 5; i++ {
		if c := attempt("owner", "10.5.5.5"); c != 401 {
			t.Fatalf("attempt %d = %d, want 401", i, c)
		}
	}
	if c := attempt("owner", "10.5.5.5"); c != 429 {
		t.Fatalf("6th attempt = %d, want 429", c)
	}
	if c := attempt("other", "10.5.5.5"); c != 401 {
		t.Fatalf("neighbour username = %d, want 401 (not 429)", c)
	}
}

// TestPGUsersIsolation — F2.2 (GitLab #20): два пользователя через login,
// расходящиеся favorites/history/later; чужая сессия → 404 (существование
// не раскрываем), admin-флаг не даёт доступа к чужим данным.
func TestPGUsersIsolation(t *testing.T) {
	server := openPGServer(t, "s3cret")
	pg, ok := server.Store.(*db.PGStore)
	if !ok {
		t.Fatalf("expect *db.PGStore, got %T", server.Store)
	}
	bID := pgtest.InsertUser(t, pg.DB, "bob", "pass-b", false)
	_ = bID
	trackID := pgtest.InsertTrack(t, pg.DB, "/iso/http1.flac", "IA", "IAL", "IT1")

	login := func(user, pass string) *http.Cookie {
		t.Helper()
		r := jsonReq("POST", "/api/auth/login", `{"username":"`+user+`","password":"`+pass+`"}`)
		r.RemoteAddr = "10.9.9.9:1"
		rec := serve(server, r)
		if rec.Code != 200 {
			t.Fatalf("login %s = %d body=%s", user, rec.Code, rec.Body.String())
		}
		c := rec.Result().Cookies()
		if len(c) != 1 {
			t.Fatalf("login %s: no cookie", user)
		}
		return c[0]
	}
	withCookie := func(method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
		r := jsonReq(method, path, body)
		r.AddCookie(c)
		return serve(server, r)
	}

	alice := login("owner", "s3cret")
	bob := login("bob", "pass-b")

	// A лайкает трек и слушает его; B ничего не видит
	if rec := withCookie("POST", "/api/favorites", `{"type":"track","track_id":`+strconv.FormatInt(trackID, 10)+`}`, alice); rec.Code != 200 {
		t.Fatalf("A favorite = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := withCookie("POST", "/api/later", `{"track_id":`+strconv.FormatInt(trackID, 10)+`}`, alice); rec.Code != 200 {
		t.Fatalf("A later = %d body=%s", rec.Code, rec.Body.String())
	}
	start := withCookie("POST", "/api/session/start", `{}`, alice)
	if start.Code != 200 {
		t.Fatalf("A session/start = %d body=%s", start.Code, start.Body.String())
	}
	var sr struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &sr); err != nil || sr.SessionID == "" {
		t.Fatalf("A session/start body: %v %s", err, start.Body.String())
	}
	if rec := withCookie("POST", "/api/events",
		`{"type":"track_end","reason":"completed","track_id":`+strconv.FormatInt(trackID, 10)+`,"session_id":"`+sr.SessionID+`"}`,
		alice); rec.Code != 200 {
		t.Fatalf("A event = %d body=%s", rec.Code, rec.Body.String())
	}

	favCount := func(c *http.Cookie) int {
		rec := withCookie("GET", "/api/favorites?type=tracks", "", c)
		if rec.Code != 200 {
			t.Fatalf("favorites = %d body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Count
	}
	if n := favCount(alice); n != 1 {
		t.Fatalf("A favorites = %d, want 1", n)
	}
	if n := favCount(bob); n != 0 {
		t.Fatalf("B favorites = %d, want 0", n)
	}

	// статус чужого лайка — false, своё позже — своё
	status := withCookie("GET", "/api/favorites/status?type=track&track_id="+strconv.FormatInt(trackID, 10), "", bob)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"favorited":false`) {
		t.Fatalf("B status = %d %s", status.Code, status.Body.String())
	}

	// история расходится: A слушала → метрики ненулевые; B — нули
	week := func(c *http.Cookie) float64 {
		rec := withCookie("GET", "/api/metrics/weekly", "", c)
		if rec.Code != 200 {
			t.Fatalf("weekly = %d", rec.Code)
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		n, _ := m["listens_7d"].(float64)
		return n
	}
	if n := week(alice); n < 1 {
		t.Fatalf("A listens_7d = %v, want >= 1", n)
	}
	if n := week(bob); n != 0 {
		t.Fatalf("B listens_7d = %v, want 0", n)
	}

	// чужая сессия → 404 (не 403 — существование не раскрываем)
	now := withCookie("GET", "/api/now?session_id="+sr.SessionID, "", bob)
	if now.Code != 404 {
		t.Fatalf("B now(A's session) = %d, want 404", now.Code)
	}
	ev := withCookie("POST", "/api/events",
		`{"type":"track_end","reason":"completed","track_id":`+strconv.FormatInt(trackID, 10)+`,"session_id":"`+sr.SessionID+`"}`,
		bob)
	if ev.Code != 404 {
		t.Fatalf("B events(A's session) = %d, want 404", ev.Code)
	}

	// B делает свой later — расходятся
	if rec := withCookie("POST", "/api/later", `{"track_id":`+strconv.FormatInt(trackID, 10)+`}`, bob); rec.Code != 200 {
		t.Fatalf("B later = %d body=%s", rec.Code, rec.Body.String())
	}
	la := withCookie("GET", "/api/later", "", alice)
	lb := withCookie("GET", "/api/later", "", bob)
	var outA, outB struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(la.Body.Bytes(), &outA)
	_ = json.Unmarshal(lb.Body.Bytes(), &outB)
	if outA.Count != 1 || outB.Count != 1 {
		t.Fatalf("later counts A=%d B=%d, want 1/1 (у каждого свой)", outA.Count, outB.Count)
	}

	// admin ≠ доступ: B становится админом — чужие favorites по-прежнему невидимы
	if _, err := pg.DB.Exec(`UPDATE users SET is_admin = TRUE WHERE username = 'bob'`); err != nil {
		t.Fatal(err)
	}
	admin := login("bob", "pass-b")
	if n := favCount(admin); n != 0 {
		t.Fatalf("admin B favorites = %d, want 0 (admin is not access)", n)
	}
	if rec := withCookie("GET", "/api/now?session_id="+sr.SessionID, "", admin); rec.Code != 404 {
		t.Fatalf("admin B now(A's session) = %d, want 404", rec.Code)
	}
}
