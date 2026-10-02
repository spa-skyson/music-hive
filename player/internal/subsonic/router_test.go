package subsonic

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// fakeStore — SubsonicStore на карте (unit-уровень, без PG).
type fakeStore struct {
	users    map[string]auth.SubsonicCredentials
	setCalls []auth.SubsonicCredentials // по одной записи на Set
}

func (f *fakeStore) AuthSubsonicCredentials(username string) (auth.SubsonicCredentials, bool, error) {
	c, ok := f.users[strings.ToLower(username)]
	return c, ok, nil
}

func (f *fakeStore) AuthSetSubsonicPassword(userID int64, md5hex, enc string) error {
	for name, c := range f.users {
		if c.User.ID != userID {
			continue
		}
		c.SubsonicMD5, c.PasswordEnc = md5hex, enc
		f.users[name] = c
		f.setCalls = append(f.setCalls, c)
		return nil
	}
	return fmt.Errorf("user %d not found", userID)
}

const testPassword = "sesame"

// newTestRouter — роутер с владельцем owner (subsonic-пароль testPassword).
func newTestRouter(t *testing.T, limit int) (*Router, *fakeStore) {
	t.Helper()
	cipher, err := auth.NewSubsonicCipher("unit-test-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := cipher.Seal(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{users: map[string]auth.SubsonicCredentials{
		"owner": {
			User:        auth.User{ID: 1, Username: "owner", IsAdmin: true, IsOwner: true},
			SubsonicMD5: auth.MD5Hex(testPassword),
			PasswordEnc: enc,
		},
	}}
	return &Router{
		Users:         store,
		Cipher:        cipher,
		Limiter:       auth.NewLoginLimiter(limit, time.Minute),
		ServerVersion: "1.0.0",
	}, store
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// authQuery — валидные параметры token+salt для owner.
func authQuery() (token, salt string) {
	salt = "abc123"
	return md5hex(testPassword + salt), salt
}

func do(rt *Router, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// envelope — разбор JSON-ответа в map (ключи верхнего уровня).
func envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	return out
}

func subResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	env := envelope(t, rec)
	sr, ok := env["subsonic-response"].(map[string]any)
	if !ok {
		t.Fatalf("no subsonic-response in %v", env)
	}
	return sr
}

// ---------------------------------------------------------------- ping

func TestPingBothPathSpellings(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	token, salt := authQuery()
	for _, path := range []string{
		"/ping?u=owner&t=" + token + "&s=" + salt + "&v=1.16.1&c=test&f=json",
		"/ping.view?u=owner&t=" + token + "&s=" + salt + "&v=1.16.1&c=test&f=json",
	} {
		rec := do(rt, "GET", path)
		if rec.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		sr := subResp(t, rec)
		if sr["status"] != "ok" {
			t.Fatalf("%s: status field=%v", path, sr["status"])
		}
		if sr["version"] != Version || sr["type"] != ServerType || sr["serverVersion"] != "1.0.0" {
			t.Fatalf("%s: envelope fields=%v", path, sr)
		}
		if sr["openSubsonic"] != true {
			t.Fatalf("%s: openSubsonic=%v", path, sr["openSubsonic"])
		}
	}
}

// TestPingXMLGolden — снапшот XML ping (по образцу .snapshots Navidrome):
// дешёвая гарантия обеих сериализаций одним DTO.
func TestPingXMLGolden(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	token, salt := authQuery()
	rec := do(rt, "GET", "/ping?u=owner&t="+token+"&s="+salt+"&v=1.16.1&c=test")
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("content-type=%s", ct)
	}
	golden := filepath.Join("testdata", "ping.xml")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if rec.Body.String() != string(want) {
		t.Fatalf("ping XML mismatch:\n got: %s\nwant: %s", rec.Body.String(), want)
	}
	// XML должен валидно парситься тем же DTO.
	var back Subsonic
	if err := xml.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Status != StatusOK || back.XMLName.Local != "subsonic-response" {
		t.Fatalf("round-trip: %+v", back)
	}
}

// ---------------------------------------------------------------- formats

func TestResponseFormats(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	token, salt := authQuery()
	base := "/getLicense?u=owner&t=" + token + "&s=" + salt + "&v=1.16.1&c=test"

	// f=json
	rec := do(rt, "GET", base+"&f=json")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("json content-type=%s", ct)
	}
	sr := subResp(t, rec)
	lic, ok := sr["license"].(map[string]any)
	if !ok || lic["valid"] != true {
		t.Fatalf("license=%v", sr["license"])
	}
	if lic["email"] != "owner" {
		t.Fatalf("license email=%v", lic["email"])
	}

	// f=jsonp с валидным callback
	rec = do(rt, "GET", base+"&f=jsonp&callback=cb.name")
	if ct := rec.Header().Get("Content-Type"); ct != "application/javascript" {
		t.Fatalf("jsonp content-type=%s", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "cb.name(") || !strings.HasSuffix(body, ")") {
		t.Fatalf("jsonp wrapper=%s", body)
	}
	inner := body[len("cb.name(") : len(body)-1]
	var wrap struct {
		Subsonic struct {
			Status  string `json:"status"`
			License struct {
				Valid bool `json:"valid"`
			} `json:"license"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal([]byte(inner), &wrap); err != nil {
		t.Fatalf("jsonp inner json: %v", err)
	}
	if wrap.Subsonic.Status != "ok" || !wrap.Subsonic.License.Valid {
		t.Fatalf("jsonp payload=%+v", wrap.Subsonic)
	}

	// jsonp с невалидным callback → HTTP 400 (анти-XSS)
	rec = do(rt, "GET", base+"&f=jsonp&callback=alert(1)")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid callback status=%d body=%s", rec.Code, rec.Body.String())
	}
	if errDesc := subResp(t, rec); errDesc["status"] != "failed" {
		t.Fatalf("invalid callback response=%v", errDesc)
	}

	// f отсутствует → XML (по спецификации)
	rec = do(rt, "GET", base)
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("xml content-type=%s", ct)
	}
	if !strings.Contains(rec.Body.String(), `valid="true"`) {
		t.Fatalf("xml body=%s", rec.Body.String())
	}
}

// TestArrayNeverNull — пустой список сериализуется как [], а не null.
func TestArrayNeverNull(t *testing.T) {
	b, err := json.Marshal(&struct {
		List Array[int32] `json:"list"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"list":[]}` {
		t.Fatalf("nil slice marshals as %s", b)
	}
}

// ---------------------------------------------------------------- middleware

func TestPostFormMergedIntoQuery(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	token, salt := authQuery()
	body := strings.Join([]string{
		"u=owner", "t=" + token, "s=" + salt, "v=1.16.1", "c=test", "f=json",
	}, "&")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ping", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rt.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || subResp(t, rec)["status"] != "ok" {
		t.Fatalf("POST ping: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMissingRequiredParameters(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	for _, q := range []string{
		"t=x&s=y&v=1.16.1&c=test&f=json",  // нет u
		"u=owner&t=x&s=y&c=test&f=json",   // нет v
		"u=owner&t=x&s=y&v=1.16.1&f=json", // нет c
	} {
		rec := do(rt, "GET", "/ping?"+q)
		sr := subResp(t, rec)
		errObj, _ := sr["error"].(map[string]any)
		if sr["status"] != "failed" || errObj == nil || errObj["code"].(float64) != 10 {
			t.Fatalf("q=%s: response=%v", q, sr)
		}
	}
}

func TestClientVersionSoftCheck(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	token, salt := authQuery()
	base := "u=owner&t=" + token + "&s=" + salt + "&c=test&f=json"

	// 1.6.0 → code 20
	rec := do(rt, "GET", "/ping?"+base+"&v=1.6.0")
	errObj, _ := subResp(t, rec)["error"].(map[string]any)
	if errObj["code"].(float64) != 20 {
		t.Fatalf("old client=%v", errObj)
	}
	// 1.8.0 и новее, странные версии — проходят
	for _, v := range []string{"1.8", "1.8.0", "1.16.1", "2.0", "weird"} {
		rec := do(rt, "GET", "/ping?"+base+"&v="+v)
		if subResp(t, rec)["status"] != "ok" {
			t.Fatalf("v=%s: %s", v, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------- auth

func TestAuthSchemes(t *testing.T) {
	rt, _ := newTestRouter(t, 100) // лимит не мешает
	cases := []struct {
		name string
		q    string
		want string // "ok" | "40"
	}{
		{"token+salt", "u=owner&t=" + md5hex(testPassword+"salt") + "&s=salt", "ok"},
		{"token+salt uppercase hex", "u=owner&t=" + strings.ToUpper(md5hex(testPassword+"salt")) + "&s=salt", "ok"},
		{"p plain", "u=owner&p=" + testPassword, "ok"},
		{"p enc hex", "u=owner&p=enc:" + hex.EncodeToString([]byte(testPassword)), "ok"},
		{"wrong token", "u=owner&t=" + md5hex("wrong"+"salt") + "&s=salt", "40"},
		{"wrong p", "u=owner&p=nope", "40"},
		{"p enc bad hex", "u=owner&p=enc:zzzz", "40"},
		{"no creds", "u=owner", "40"},
		{"unknown user", "u=ghost&t=abc&s=s", "40"},
		{"case-insensitive username", "u=OWNER&p=" + testPassword, "ok"},
	}
	for _, tc := range cases {
		rec := do(rt, "GET", "/ping?"+tc.q+"&v=1.16.1&c=test&f=json")
		sr := subResp(t, rec)
		if tc.want == "ok" && sr["status"] != "ok" {
			t.Fatalf("%s: %v", tc.name, sr)
		}
		if tc.want == "40" {
			errObj, _ := sr["error"].(map[string]any)
			if sr["status"] != "failed" || errObj["code"].(float64) != 40 {
				t.Fatalf("%s: %v", tc.name, sr)
			}
		}
	}
}

func TestAuthDisabledUserSameAsWrongPassword(t *testing.T) {
	rt, store := newTestRouter(t, 5)
	c := store.users["owner"]
	c.Disabled = true
	store.users["owner"] = c
	rec := do(rt, "GET", "/ping?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json")
	errObj, _ := subResp(t, rec)["error"].(map[string]any)
	if errObj["code"].(float64) != 40 {
		t.Fatalf("disabled user=%v", errObj)
	}
}

func TestAuthRateLimitMasksAs40(t *testing.T) {
	rt, _ := newTestRouter(t, 2)
	req := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/ping?u=owner&p=wrong&v=1.16.1&c=test&f=json", nil)
		r.RemoteAddr = "10.1.1.1:7" // стабильный ключ IP|username
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, r)
		return rec
	}
	for i := 0; i < 2; i++ { // две неудачи — лимит исчерпан
		req()
	}
	rec := do(rt, "GET", "/ping?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json")
	// GET без RemoteAddr → 192.0.2.1 (другой IP) — не задет лимитом
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("unrelated IP blocked: %s", rec.Body.String())
	}
	// тот же IP: даже верный пароль → 40 (маскировка блокировки)
	r := httptest.NewRequest("GET", "/ping?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json", nil)
	r.RemoteAddr = "10.1.1.1:7"
	rec2 := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec2, r)
	errObj, _ := subResp(t, rec2)["error"].(map[string]any)
	if errObj["code"].(float64) != 40 {
		t.Fatalf("rate-limited=%v", errObj)
	}
	// успешные запросы лимит не тратят: другой юзер с того же IP работает
	store := rt.Users.(*fakeStore)
	store.users["alice"] = store.users["owner"]
	r = httptest.NewRequest("GET", "/ping?u=alice&p="+testPassword+"&v=1.16.1&c=test&f=json", nil)
	r.RemoteAddr = "10.1.1.1:7"
	rec3 := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec3, r)
	if subResp(t, rec3)["status"] != "ok" {
		t.Fatalf("success consumed limit: %s", rec3.Body.String())
	}
}

func TestSubsonicAuthDisabledFlag(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	rt.Disabled = true
	rec := do(rt, "GET", "/ping?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json")
	sr := subResp(t, rec)
	errObj, _ := sr["error"].(map[string]any)
	if sr["status"] != "failed" || errObj["code"].(float64) != 50 {
		t.Fatalf("disabled flag=%v", sr)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "MUSIC_HIVE_SUBSONIC_AUTH=0") {
		t.Fatalf("no explanation: %v", errObj)
	}
	// публичный эндпоинт остаётся доступным
	rec = do(rt, "GET", "/getOpenSubsonicExtensions?f=json")
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("public endpoint blocked: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------- endpoints

func TestGetOpenSubsonicExtensions(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	rec := do(rt, "GET", "/getOpenSubsonicExtensions?f=json") // без u/t/p — публичный
	sr := subResp(t, rec)
	exts, ok := sr["openSubsonicExtensions"].([]any)
	if !ok || len(exts) != 1 {
		t.Fatalf("extensions=%v", sr["openSubsonicExtensions"])
	}
	ext := exts[0].(map[string]any)
	if ext["name"] != "formPost" {
		t.Fatalf("ext=%v", ext)
	}
	versions, _ := ext["versions"].([]any)
	if len(versions) != 1 || versions[0].(float64) != 1 {
		t.Fatalf("versions=%v", ext["versions"])
	}
	// .view-написание тоже публично
	rec = do(rt, "GET", "/getOpenSubsonicExtensions.view?f=json")
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf(".view spelling failed: %s", rec.Body.String())
	}
}

// UnknownPath → 404 mux'а (не конверт): незарегистрированный эндпоинт
// (getArtists и прочие с F3.2 уже в реестре).
func TestUnknownPath(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	rec := do(rt, "GET", "/getVideos?u=owner&p=x&v=1.16.1&c=t&f=json")
	if rec.Code != 404 {
		t.Fatalf("unknown path=%d", rec.Code)
	}
}

// SetPasswordThroughAPI — контракт Store: Set обновляет обе колонки.
func TestSetPasswordThroughStore(t *testing.T) {
	rt, store := newTestRouter(t, 5)
	if err := store.AuthSetSubsonicPassword(1, auth.MD5Hex("newpw"), mustSeal(t, rt, "newpw")); err != nil {
		t.Fatal(err)
	}
	rec := do(rt, "GET", "/ping?u=owner&p=newpw&v=1.16.1&c=test&f=json")
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("new password rejected: %s", rec.Body.String())
	}
	// старый пароль больше не работает
	rec = do(rt, "GET", "/ping?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json")
	if subResp(t, rec)["status"] == "ok" {
		t.Fatal("old password still valid")
	}
}

func mustSeal(t *testing.T, rt *Router, clear string) string {
	t.Helper()
	enc, err := rt.Cipher.Seal(clear)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}
