package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
)

// F2.3 (GitLab #21): admin-API управления пользователями + свои токены.
// PG-паттерн: openPGServer (pgtest.Open → BootstrapOwner → api.New).

// loginCookie — логин, возвращает cookie сессии (fatal при неудаче).
func loginCookie(t *testing.T, server *api.Server, username, password string) *http.Cookie {
	t.Helper()
	r := jsonReq("POST", "/api/auth/login", `{"username":"`+username+`","password":"`+password+`"}`)
	r.RemoteAddr = "10.8.8.8:1"
	rec := serve(server, r)
	if rec.Code != 200 {
		t.Fatalf("login %s = %d body=%s", username, rec.Code, rec.Body.String())
	}
	c := rec.Result().Cookies()
	if len(c) != 1 {
		t.Fatalf("login %s: no cookie", username)
	}
	return c[0]
}

// serveWith — запрос с cookie сессии.
func serveWith(server *api.Server, method, path, body string, c *http.Cookie) *httptest.ResponseRecorder {
	r := jsonReq(method, path, body)
	r.AddCookie(c)
	return serve(server, r)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// createBobViaAdmin — админ заводит bob, возвращает его свежую сессию.
func createBobViaAdmin(t *testing.T, server *api.Server, admin *http.Cookie, password string) *http.Cookie {
	t.Helper()
	rec := serveWith(server, "POST", "/api/admin/users", `{"username":"bob","password":"`+password+`"}`, admin)
	if rec.Code != 200 {
		t.Fatalf("create bob = %d body=%s", rec.Code, rec.Body.String())
	}
	return loginCookie(t, server, "bob", password)
}

// TestPGAdminUsersLifecycle: админ создаёт bob → bob логинится → админ
// выключает → сессия bob немедленно 401, логин 403 → включают → логин ок.
// Не-админ → 403; owner нельзя выключить/разжаловать → 400; дубликат и
// плохое имя → 400; в списке нет хешей.
func TestPGAdminUsersLifecycle(t *testing.T) {
	server := openPGServer(t, "s3cret")
	admin := loginCookie(t, server, "owner", "s3cret")
	withAdmin := func(method, path, body string) *httptest.ResponseRecorder {
		return serveWith(server, method, path, body, admin)
	}

	// создать bob (не админ)
	rec := withAdmin("POST", "/api/admin/users", `{"username":"bob","password":"bob-pass-1"}`)
	if rec.Code != 200 {
		t.Fatalf("create bob = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		User struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			IsAdmin  bool   `json:"is_admin"`
			Disabled bool   `json:"disabled"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.User.Username != "bob" ||
		created.User.ID == 0 || created.User.IsAdmin || created.User.Disabled {
		t.Fatalf("created bob: %v %s", err, rec.Body.String())
	}
	bobID := created.User.ID

	// валидация имени, дубликат, пустой пароль
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"username":"X","password":"p"}`, 400},
		{`{"username":"ab","password":"p"}`, 400},
		{`{"username":"bob","password":"p"}`, 400},
		{`{"username":"carol","password":""}`, 400},
	} {
		if rec := withAdmin("POST", "/api/admin/users", tc.body); rec.Code != tc.want {
			t.Fatalf("create %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}

	// список: без хешей, bob на месте
	rec = withAdmin("GET", "/api/admin/users", "")
	if rec.Code != 200 {
		t.Fatalf("list = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), "password_argon2") {
		t.Fatalf("user list leaks hashes: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"username":"bob"`) {
		t.Fatalf("bob missing from list: %s", rec.Body.String())
	}

	// bob логинится и НЕ админ: все admin-роуты → 403
	bob := loginCookie(t, server, "bob", "bob-pass-1")
	if rec := serveWith(server, "GET", "/api/admin/users", "", bob); rec.Code != 403 {
		t.Fatalf("bob list users = %d, want 403", rec.Code)
	}
	if rec := serveWith(server, "POST", "/api/admin/users", `{"username":"eve","password":"p"}`, bob); rec.Code != 403 {
		t.Fatalf("bob create user = %d, want 403", rec.Code)
	}
	if rec := serveWith(server, "PATCH", "/api/admin/users/1", `{"disabled":true}`, bob); rec.Code != 403 {
		t.Fatalf("bob patch user = %d, want 403", rec.Code)
	}
	// гость (без аутентификации) — 401 от мидлвари
	if rec := serve(server, jsonReq("GET", "/api/admin/users", "")); rec.Code != 401 {
		t.Fatalf("anon list users = %d, want 401", rec.Code)
	}

	// owner защищён
	if rec := withAdmin("PATCH", "/api/admin/users/1", `{"disabled":true}`); rec.Code != 400 {
		t.Fatalf("disable owner = %d, want 400", rec.Code)
	}
	if rec := withAdmin("PATCH", "/api/admin/users/1", `{"is_admin":false}`); rec.Code != 400 {
		t.Fatalf("demote owner = %d, want 400", rec.Code)
	}
	// пустой патч и несуществующий id
	if rec := withAdmin("PATCH", "/api/admin/users/"+itoa(bobID), `{}`); rec.Code != 400 {
		t.Fatalf("empty patch = %d, want 400", rec.Code)
	}
	if rec := withAdmin("PATCH", "/api/admin/users/999999", `{"disabled":true}`); rec.Code != 404 {
		t.Fatalf("patch unknown = %d, want 404", rec.Code)
	}

	// админ выключает bob → его сессия немедленно 401
	if rec := withAdmin("PATCH", "/api/admin/users/"+itoa(bobID), `{"disabled":true}`); rec.Code != 200 {
		t.Fatalf("disable bob = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := serveWith(server, "GET", "/api/status", "", bob); rec.Code != 401 {
		t.Fatalf("disabled bob session = %d, want immediate 401", rec.Code)
	}
	// логин выключенного: верный пароль → 403 account_disabled
	disReq := jsonReq("POST", "/api/auth/login", `{"username":"bob","password":"bob-pass-1"}`)
	disReq.RemoteAddr = "10.8.8.9:1"
	if rec := serve(server, disReq); rec.Code != 403 {
		t.Fatalf("disabled bob login = %d body=%s, want 403", rec.Code, rec.Body.String())
	}

	// включают → логин снова ок
	if rec := withAdmin("PATCH", "/api/admin/users/"+itoa(bobID), `{"disabled":false}`); rec.Code != 200 {
		t.Fatalf("enable bob = %d body=%s", rec.Code, rec.Body.String())
	}
	loginCookie(t, server, "bob", "bob-pass-1")

	// сброс пароля админом: старый не работает, новый работает
	if rec := withAdmin("PATCH", "/api/admin/users/"+itoa(bobID), `{"password":"bob-pass-2"}`); rec.Code != 200 {
		t.Fatalf("reset bob password = %d", rec.Code)
	}
	oldReq := jsonReq("POST", "/api/auth/login", `{"username":"bob","password":"bob-pass-1"}`)
	oldReq.RemoteAddr = "10.8.8.10:1"
	if rec := serve(server, oldReq); rec.Code != 401 {
		t.Fatalf("old bob password = %d, want 401", rec.Code)
	}
	loginCookie(t, server, "bob", "bob-pass-2")

	// назначение админа: bob получает доступ к списку
	if rec := withAdmin("PATCH", "/api/admin/users/"+itoa(bobID), `{"is_admin":true}`); rec.Code != 200 {
		t.Fatalf("promote bob = %d", rec.Code)
	}
	bob2 := loginCookie(t, server, "bob", "bob-pass-2")
	if rec := serveWith(server, "GET", "/api/admin/users", "", bob2); rec.Code != 200 {
		t.Fatalf("admin bob list = %d, want 200", rec.Code)
	}
}

// TestPGOwnTokensLifecycle: список своих токенов (префикс, без секрета),
// отзыв делает Bearer мёртвым, чужой токен неотзываем (404).
func TestPGOwnTokensLifecycle(t *testing.T) {
	server := openPGServer(t, "s3cret")
	owner := loginCookie(t, server, "owner", "s3cret")

	// два токена
	var toks [2]struct {
		Token string `json:"token"`
	}
	for i := range toks {
		rec := serveWith(server, "POST", "/api/auth/tokens", `{"name":"t`+itoa(int64(i))+`"}`, owner)
		if rec.Code != 200 {
			t.Fatalf("create token = %d body=%s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &toks[i]); err != nil {
			t.Fatal(err)
		}
	}

	rec := serveWith(server, "GET", "/api/auth/tokens", "", owner)
	if rec.Code != 200 {
		t.Fatalf("list tokens = %d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Tokens []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tokens) != 2 {
		t.Fatalf("tokens=%d, want 2", len(list.Tokens))
	}
	for i, ti := range list.Tokens {
		want := "t" + itoa(int64(1-i)) // DESC: второй создан позже
		if ti.Name != want {
			t.Fatalf("token[%d].name=%q, want %q", i, ti.Name, want)
		}
		if !strings.HasPrefix(toks[1-i].Token, ti.Prefix) || ti.Prefix == "" {
			t.Fatalf("token[%d].prefix=%q must be a prefix of the secret", i, ti.Prefix)
		}
	}
	if strings.Contains(rec.Body.String(), toks[0].Token) || strings.Contains(rec.Body.String(), toks[1].Token) {
		t.Fatal("token list leaks secrets")
	}

	// отзыв: Bearer умирает, повтор — 404, второй жив
	id := list.Tokens[1].ID // соответствует toks[0]
	if rec := serveWith(server, "DELETE", "/api/auth/tokens/"+itoa(id), "", owner); rec.Code != 200 {
		t.Fatalf("revoke = %d", rec.Code)
	}
	dead := jsonReq("GET", "/api/auth/me", "")
	dead.Header.Set("Authorization", "Bearer "+toks[0].Token)
	if rec := serve(server, dead); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("revoked bearer me = %d %s, want 200/ok:false", rec.Code, rec.Body.String())
	}
	if rec := serveWith(server, "DELETE", "/api/auth/tokens/"+itoa(id), "", owner); rec.Code != 404 {
		t.Fatalf("double revoke = %d, want 404", rec.Code)
	}
	alive := jsonReq("GET", "/api/auth/me", "")
	alive.Header.Set("Authorization", "Bearer "+toks[1].Token)
	if rec := serve(server, alive); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("surviving bearer me = %d %s", rec.Code, rec.Body.String())
	}

	// bob: свой список пуст, чужой id не отзывается (404)
	bob := createBobViaAdmin(t, server, owner, "bob-pass-1")
	if rec := serveWith(server, "GET", "/api/auth/tokens", "", bob); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"tokens":[]`) {
		t.Fatalf("bob tokens = %d %s, want empty list", rec.Code, rec.Body.String())
	}
	if rec := serveWith(server, "DELETE", "/api/auth/tokens/"+itoa(list.Tokens[0].ID), "", bob); rec.Code != 404 {
		t.Fatalf("bob revokes owner token = %d, want 404", rec.Code)
	}
}
