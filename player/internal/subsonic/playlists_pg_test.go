// Живые PG-тесты эндпоинтов F3.4 (#26): плейлисты CRUD (изоляция user
// A/B), play-queue persist/reload, bookmarks, интернет-радио (права
// owner/admin/гость), getUser(s)/getScanStatus/startScan, getNowPlaying,
// getSimilarSongs и приёмочная цепочка «клиент». Helpers общие с
// catalog_pg_test.go / media_pg_test.go (тот же пакет).

package subsonic_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// bobQuery — идемпотентный bob (не админ): возвращает auth-суффикс
// query (bobDo из media_pg_test.go вставляет юзера при каждом вызове).
func bobQuery(t *testing.T, store *db.PGStore) string {
	t.Helper()
	hash, err := auth.HashPassword(bobPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	var id int64
	if err := store.DB.QueryRow(`
INSERT INTO users (username, password_argon2, is_admin)
VALUES ('bob', $1, FALSE)
ON CONFLICT (username) DO UPDATE SET password_argon2 = EXCLUDED.password_argon2
RETURNING id`, hash).Scan(&id); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := store.DB.Exec(
		`UPDATE users SET subsonic_md5 = $1 WHERE id = $2`, auth.MD5Hex(bobPassword), id); err != nil {
		t.Fatalf("seed bob creds: %v", err)
	}
	return "u=bob&p=" + bobPassword + "&v=1.16.1&c=test&f=json"
}

// bobReq — запрос от bob (bobQuery уже вызван).
func bobReq(rt *subsonic.Router, creds, path string) *httptest.ResponseRecorder {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return do(rt, "GET", path+sep+creds)
}

// ---------------------------------------------------------------- плейлисты

func TestSubsonicPlaylistCRUD(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)
	bq := bobQuery(t, store)

	// пусто
	list := asMap(t, subResp(t, catDo(rt, "/getPlaylists"))["playlists"])
	if got := asList(t, list["playlist"]); len(got) != 0 {
		t.Fatalf("fresh playlists=%v", got)
	}

	// создание с двумя треками
	pl := asMap(t, subResp(t, catDo(rt,
		fmt.Sprintf("/createPlaylist?name=Road&songId=%d&songId=%d", s.tracks[0], s.tracks[1])))["playlist"])
	if pl["id"] == "" || pl["name"] != "Road" || pl["owner"] != "owner" {
		t.Fatalf("created=%v", pl)
	}
	id := pl["id"].(string)

	// getPlaylist с треками
	full := asMap(t, subResp(t, catDo(rt, "/getPlaylist?id="+id))["playlist"])
	if full["songCount"].(float64) != 2 || full["duration"].(float64) != 580 {
		t.Fatalf("full=%v", full)
	}
	if got := asList(t, full["entry"]); len(got) != 2 || asMap(t, got[0])["id"] != fmt.Sprint(s.tracks[0]) {
		t.Fatalf("entries=%v", got)
	}

	// список видит плейлист
	list = asMap(t, subResp(t, catDo(rt, "/getPlaylists"))["playlists"])
	if got := asList(t, list["playlist"]); len(got) != 1 || asMap(t, got[0])["id"] != id {
		t.Fatalf("list=%v", got)
	}

	// update: rename + comment + добавить boards-трек + убрать первую позицию
	rec := catDo(rt, fmt.Sprintf(
		"/updatePlaylist?playlistId=%s&name=%s&comment=favorite&songIdToAdd=%d&songIndexToRemove=0",
		id, url.QueryEscape("Road 2"), s.tracks[4]))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("update: %s", rec.Body)
	}
	full = asMap(t, subResp(t, catDo(rt, "/getPlaylist?id="+id))["playlist"])
	if full["name"] != "Road 2" || full["comment"] != "favorite" {
		t.Fatalf("after update=%v", full)
	}
	got := asList(t, full["entry"])
	if len(got) != 2 || asMap(t, got[0])["id"] != fmt.Sprint(s.tracks[1]) ||
		asMap(t, got[1])["id"] != fmt.Sprint(s.tracks[4]) {
		t.Fatalf("after update entries=%v", got)
	}

	// createPlaylist с playlistId — полная замена состава
	rec = catDo(rt, fmt.Sprintf("/createPlaylist?playlistId=%s&songId=%d", id, s.tracks[2]))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("replace: %s", rec.Body)
	}
	full = asMap(t, subResp(t, catDo(rt, "/getPlaylist?id="+id))["playlist"])
	if got := asList(t, full["entry"]); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprint(s.tracks[2]) {
		t.Fatalf("after replace=%v", got)
	}

	// изоляция: bob не видит, не меняет и не удаляет чужой плейлист (70)
	wantCode(t, bobReq(rt, bq, "/getPlaylist?id="+id), 70)
	wantCode(t, bobReq(rt, bq, "/updatePlaylist?playlistId="+id+"&name=Hacked"), 70)
	wantCode(t, bobReq(rt, bq, "/deletePlaylist?id="+id), 70)
	// своих плейлистов нет, свой создать/удалить может
	if bob := asMap(t, subResp(t, bobReq(rt, bq, "/getPlaylists"))["playlists"]); len(asList(t, bob["playlist"])) != 0 {
		t.Fatal("bob sees owner playlists")
	}
	bobPl := asMap(t, subResp(t, bobReq(rt, bq, "/createPlaylist?name=BobMix"))["playlist"])
	if rec := bobReq(rt, bq, "/deletePlaylist?id="+bobPl["id"].(string)); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("bob delete own: %s", rec.Body)
	}

	// удаление владельцем
	if rec := catDo(rt, "/deletePlaylist?id="+id); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("delete: %s", rec.Body)
	}
	wantCode(t, catDo(rt, "/getPlaylist?id="+id), 70)

	// ошибки
	wantCode(t, catDo(rt, "/createPlaylist"), 10)                                  // нет name
	wantCode(t, catDo(rt, "/createPlaylist?name=X&songId=999"), 70)                // нет трека
	wantCode(t, catDo(rt, "/createPlaylist?name=X&songId=abc"), 70)                // мусорный id
	wantCode(t, catDo(rt, "/updatePlaylist?playlistId=999&name=Y"), 70)            // нет плейлиста
	wantCode(t, catDo(rt, "/getPlaylist"), 10)                                     // нет id
	wantCode(t, catDo(rt, "/updatePlaylist?playlistId=1&songIndexToRemove=x"), 10) // мусорный индекс
}

// ---------------------------------------------------------------- play-queue

func TestSubsonicPlayQueuePersistReload(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)
	bq := bobQuery(t, store)

	// ничего не сохранено — пустой ok без playQueue
	sr := subResp(t, catDo(rt, "/getPlayQueue"))
	if sr["status"] != "ok" {
		t.Fatalf("empty queue: %v", sr)
	}
	if _, has := sr["playQueue"]; has {
		t.Fatalf("unexpected playQueue: %v", sr["playQueue"])
	}

	// сохранить
	rec := catDo(rt, fmt.Sprintf(
		"/savePlayQueue?songId=%d&songId=%d&currentIndex=1&position=42000&changed=2026-09-25T10:00:00Z",
		s.tracks[0], s.tracks[1]))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("save: %s", rec.Body)
	}

	// строка play_sessions с queue_json и mode=subsonic (jsonb нормализует
	// пробелы/порядок ключей — сверяем разбором)
	var mode, qj string
	if err := store.DB.QueryRow(
		`SELECT mode, queue_json::text FROM play_sessions WHERE id = 'subsonic-1'`).
		Scan(&mode, &qj); err != nil {
		t.Fatalf("play_sessions row: %v", err)
	}
	if mode != "subsonic" {
		t.Fatalf("row mode=%s qj=%s", mode, qj)
	}
	var saved struct {
		ChangedBy string  `json:"changedBy"`
		Position  float64 `json:"position"`
		IDs       []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(qj), &saved); err != nil || saved.ChangedBy != "test" ||
		saved.Position != 42000 || len(saved.IDs) != 2 {
		t.Fatalf("queue_json=%s err=%v", qj, err)
	}

	// перечитать (persist/reload)
	pq := asMap(t, subResp(t, catDo(rt, "/getPlayQueue"))["playQueue"])
	if pq["username"] != "owner" || pq["current"] != fmt.Sprint(s.tracks[1]) ||
		pq["position"].(float64) != 42000 || pq["changedBy"] != "test" {
		t.Fatalf("queue=%v", pq)
	}
	if got := asList(t, pq["entry"]); len(got) != 2 {
		t.Fatalf("entries=%v", got)
	}

	// изоляция: у bob очередь пуста, своя — своя
	if sr := subResp(t, bobReq(rt, bq, "/getPlayQueue")); sr["playQueue"] != nil {
		t.Fatalf("bob sees owner queue: %v", sr["playQueue"])
	}
	rec = bobReq(rt, bq, fmt.Sprintf("/savePlayQueue?songId=%d", s.tracks[3]))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("bob save: %s", rec.Body)
	}
	bpq := asMap(t, subResp(t, bobReq(rt, bq, "/getPlayQueue"))["playQueue"])
	if bpq["username"] != "bob" || len(asList(t, bpq["entry"])) != 1 {
		t.Fatalf("bob queue=%v", bpq)
	}

	// ошибки
	wantCode(t, catDo(rt, "/savePlayQueue"), 10)            // нет songId
	wantCode(t, catDo(rt, "/savePlayQueue?songId=999"), 70) // нет трека
	wantCode(t, catDo(rt, "/savePlayQueue?songId=abc"), 70) // мусор
}

// ---------------------------------------------------------------- bookmarks

func TestSubsonicBookmarks(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)
	bq := bobQuery(t, store)

	// пусто
	bm := asMap(t, subResp(t, catDo(rt, "/getBookmarks"))["bookmarks"])
	if got := asList(t, bm["bookmark"]); len(got) != 0 {
		t.Fatalf("fresh bookmarks=%v", got)
	}

	// создать (position/comment принимаются, но не хранятся — матрица)
	if rec := catDo(rt, fmt.Sprintf("/createBookmark?id=%d&position=123&comment=later", s.tracks[0])); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("create: %s", rec.Body)
	}
	bm = asMap(t, subResp(t, catDo(rt, "/getBookmarks"))["bookmarks"])
	got := asList(t, bm["bookmark"])
	if len(got) != 1 {
		t.Fatalf("bookmarks=%v", got)
	}
	entry := asMap(t, got[0])
	if entry["username"] != "owner" || entry["position"].(float64) != 0 ||
		entry["id"] != fmt.Sprint(s.tracks[0]) {
		t.Fatalf("entry=%v", entry)
	}
	// строка user_listen_later на месте (маппинг «послушать позже»)
	var n int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM user_listen_later WHERE user_id = 1 AND track_id = $1`, s.tracks[0]).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("listen_later n=%d err=%v", n, err)
	}

	// изоляция: у bob пусто
	if bob := asMap(t, subResp(t, bobReq(rt, bq, "/getBookmarks"))["bookmarks"]); len(asList(t, bob["bookmark"])) != 0 {
		t.Fatal("bob sees owner bookmarks")
	}

	// удалить
	if rec := catDo(rt, fmt.Sprintf("/deleteBookmark?id=%d", s.tracks[0])); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("delete: %s", rec.Body)
	}
	bm = asMap(t, subResp(t, catDo(rt, "/getBookmarks"))["bookmarks"])
	if got := asList(t, bm["bookmark"]); len(got) != 0 {
		t.Fatalf("after delete=%v", got)
	}

	// ошибки
	wantCode(t, catDo(rt, "/createBookmark"), 10)        // нет id
	wantCode(t, catDo(rt, "/createBookmark?id=999"), 70) // нет трека
	wantCode(t, catDo(rt, "/deleteBookmark?id=abc"), 70) // мусор
}

// ---------------------------------------------------------------- radio

func TestSubsonicInternetRadioRights(t *testing.T) {
	rt, _, store := newMediaTestRouter(t)
	bq := bobQuery(t, store)

	// пусто
	st := asMap(t, subResp(t, catDo(rt, "/getInternetRadioStations"))["internetRadioStations"])
	if got := asList(t, st["internetRadioStation"]); len(got) != 0 {
		t.Fatalf("fresh stations=%v", got)
	}

	// создать: streamUrl/homepageUrl сервер генерирует сам (внешние игнорируются)
	if rec := catDo(rt, "/createInternetRadioStation?name=Kitchen&streamUrl=http://x&homepageUrl=http://y"); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("create: %s", rec.Body)
	}
	st = asMap(t, subResp(t, catDo(rt, "/getInternetRadioStations"))["internetRadioStations"])
	got := asList(t, st["internetRadioStation"])
	if len(got) != 1 {
		t.Fatalf("stations=%v", got)
	}
	station := asMap(t, got[0])
	token := station["id"].(string)
	if station["name"] != "Kitchen" || station["streamUrl"] != "https://mh.example/listen/"+token+".mp3" ||
		station["homepageUrl"] != "https://mh.example" {
		t.Fatalf("station=%v", station)
	}

	// переименовать владелец может, bob — нет (50)
	if rec := catDo(rt, "/updateInternetRadioStation?id="+token+"&name=Garage"); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("rename: %s", rec.Body)
	}
	wantCode(t, bobReq(rt, bq, "/updateInternetRadioStation?id="+token+"&name=Hack"), 50)

	// bob создаёт свою — видит только свою
	if rec := bobReq(rt, bq, "/createInternetRadioStation?name=BobFM"); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("bob create: %s", rec.Body)
	}
	bobSt := asMap(t, subResp(t, bobReq(rt, bq, "/getInternetRadioStations"))["internetRadioStations"])
	if name := asMap(t, asList(t, bobSt["internetRadioStation"])[0])["name"]; name != "BobFM" {
		t.Fatalf("bob stations=%v", bobSt)
	}

	// удалить: bob чужую — 50, владелец свою — ок; повторно — 70 (отозвана)
	wantCode(t, bobReq(rt, bq, "/deleteInternetRadioStation?id="+token), 50)
	if rec := catDo(rt, "/deleteInternetRadioStation?id="+token); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("delete: %s", rec.Body)
	}
	wantCode(t, catDo(rt, "/deleteInternetRadioStation?id="+token), 70)
	st = asMap(t, subResp(t, catDo(rt, "/getInternetRadioStations"))["internetRadioStations"])
	if got := asList(t, st["internetRadioStation"]); len(got) != 0 {
		t.Fatalf("after delete=%v", got)
	}

	// ошибки
	wantCode(t, catDo(rt, "/createInternetRadioStation"), 10) // нет name
	wantCode(t, catDo(rt, "/updateInternetRadioStation?id=zz&name=X"), 70)
	wantCode(t, catDo(rt, "/deleteInternetRadioStation"), 10)
}

// ---------------------------------------------------------------- users/scan/avatar

func TestSubsonicUsersScanAvatar(t *testing.T) {
	rt, _, store := newMediaTestRouter(t)
	bq := bobQuery(t, store)

	// getUser без параметров — свой блок
	u := asMap(t, subResp(t, catDo(rt, "/getUser"))["user"])
	if u["username"] != "owner" || u["adminRole"] != true || u["streamRole"] != true ||
		u["jukeboxRole"] != false || u["shareRole"] != true || u["folder"].([]any)[0] != float64(1) {
		t.Fatalf("user=%v", u)
	}

	// bob: себя может, owner — 50; getUsers/startScan — 50
	if u := asMap(t, subResp(t, bobReq(rt, bq, "/getUser"))["user"]); u["adminRole"] != false || u["shareRole"] != false {
		t.Fatalf("bob user=%v", u)
	}
	wantCode(t, bobReq(rt, bq, "/getUser?username=owner"), 50)
	wantCode(t, bobReq(rt, bq, "/getUsers"), 50)
	wantCode(t, bobReq(rt, bq, "/startScan"), 50)

	// owner: getUsers со списком, getUser по имени
	users := asMap(t, subResp(t, catDo(rt, "/getUsers"))["users"])
	if got := asList(t, users["user"]); len(got) < 2 {
		t.Fatalf("users=%v", got)
	}
	if u := asMap(t, subResp(t, catDo(rt, "/getUser?username=bob"))["user"]); u["username"] != "bob" {
		t.Fatalf("getUser bob=%v", u)
	}
	wantCode(t, catDo(rt, "/getUser?username=nobody"), 70)

	// scan status: 5 треков сида, не сканирует
	sc := asMap(t, subResp(t, catDo(rt, "/getScanStatus"))["scanStatus"])
	if sc["count"].(float64) != 5 || sc["scanning"] != false {
		t.Fatalf("scan=%v", sc)
	}
	// startScan ставит full_rescan → scanning
	if rec := catDo(rt, "/startScan"); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("startScan: %s", rec.Body)
	}
	sc = asMap(t, subResp(t, catDo(rt, "/getScanStatus"))["scanStatus"])
	if sc["scanning"] != true {
		t.Fatalf("after startScan=%v", sc)
	}
	var status string
	if err := store.DB.QueryRow(
		`SELECT status FROM jobs WHERE kind = 'full_rescan' ORDER BY id DESC`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("job status=%s err=%v", status, err)
	}

	// аватаров нет → 70; getTopSongs → 501
	wantCode(t, catDo(rt, "/getAvatar?username=owner"), 70)
	if rec := catDo(rt, "/getTopSongs?artist=X"); rec.Code != 501 {
		t.Fatalf("topSongs=%d", rec.Code)
	}
}

// ---------------------------------------------------------------- now playing

func TestSubsonicNowPlaying(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)

	// активная сессия плеера владельца + subsonic-очередь (не «играет») +
	// старая сессия (>10 мин — не показывается)
	if _, err := store.DB.Exec(`
INSERT INTO play_sessions(id, user_id, mode, current_id, updated_at)
VALUES ('web-1', 1, 'session', $1, now())`, s.tracks[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`
INSERT INTO play_sessions(id, user_id, mode, current_id, updated_at)
VALUES ('web-old', 1, 'session', $1, now() - interval '1 hour')`, s.tracks[1]); err != nil {
		t.Fatal(err)
	}
	if rec := catDo(rt, fmt.Sprintf("/savePlayQueue?songId=%d", s.tracks[2])); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("save queue: %s", rec.Body)
	}

	np := asMap(t, subResp(t, catDo(rt, "/getNowPlaying"))["nowPlaying"])
	got := asList(t, np["entry"])
	if len(got) != 1 {
		t.Fatalf("entries=%v", got)
	}
	entry := asMap(t, got[0])
	if entry["username"] != "owner" || entry["id"] != fmt.Sprint(s.tracks[0]) || entry["minutesAgo"] != float64(0) {
		t.Fatalf("entry=%v", entry)
	}
}

// ---------------------------------------------------------------- similar

func TestSubsonicSimilarSongs(t *testing.T) {
	rt, s, _ := newMediaTestRouter(t)
	var lastCount int
	rt.Similar = func(kind string, id int64, count int) []int64 {
		if kind != "track" || id != s.tracks[0] {
			t.Errorf("similar args: kind=%s id=%d", kind, id)
		}
		lastCount = count
		return []int64{s.tracks[1], s.tracks[4]}
	}

	songs := asMap(t, subResp(t, catDo(rt, fmt.Sprintf("/getSimilarSongs?id=%d", s.tracks[0])))["similarSongs"])
	if lastCount != 50 {
		t.Fatalf("default count=%d want 50", lastCount)
	}
	got := asList(t, songs["song"])
	if len(got) != 2 || asMap(t, got[0])["id"] != fmt.Sprint(s.tracks[1]) {
		t.Fatalf("songs=%v", got)
	}
	// ID3-форма — то же тело под другим элементом конверта, count на месте
	songs = asMap(t, subResp(t, catDo(rt, fmt.Sprintf("/getSimilarSongs2?id=%d&count=3", s.tracks[0])))["similarSongs2"])
	if lastCount != 3 || len(asList(t, songs["song"])) != 2 {
		t.Fatalf("songs2 count=%d=%v", lastCount, songs)
	}

	// нет сущности → 70; нет id → 10
	wantCode(t, catDo(rt, "/getSimilarSongs?id=999"), 70)
	wantCode(t, catDo(rt, "/getSimilarSongs"), 10)

	// similar-функция не задана → 501
	rt.Similar = nil
	if rec := catDo(rt, fmt.Sprintf("/getSimilarSongs?id=%d", s.tracks[0])); rec.Code != 501 {
		t.Fatalf("nil similar=%d", rec.Code)
	}
}

// ---------------------------------------------------------------- chain

// TestSubsonicClientChain — приёмочная цепочка «клиент» (#26): пустой
// список → create → get → update → savePlayQueue → getPlayQueue → delete.
func TestSubsonicClientChain(t *testing.T) {
	rt, s, _ := newMediaTestRouter(t)

	// 1. пустой список
	list := asMap(t, subResp(t, catDo(rt, "/getPlaylists"))["playlists"])
	if got := asList(t, list["playlist"]); len(got) != 0 {
		t.Fatalf("step1=%v", got)
	}

	// 2. создать
	pl := asMap(t, subResp(t, catDo(rt,
		fmt.Sprintf("/createPlaylist?name=Trip&songId=%d&songId=%d&songId=%d",
			s.tracks[0], s.tracks[2], s.tracks[4])))["playlist"])
	id := pl["id"].(string)

	// 3. получить с треками
	full := asMap(t, subResp(t, catDo(rt, "/getPlaylist?id="+id))["playlist"])
	if full["songCount"].(float64) != 3 {
		t.Fatalf("step3=%v", full)
	}
	ids := make([]string, 0, 3)
	for _, raw := range asList(t, full["entry"]) {
		ids = append(ids, asMap(t, raw)["id"].(string))
	}

	// 4. обновить (переименовать и убрать середину)
	if rec := catDo(rt, "/updatePlaylist?playlistId="+id+"&name="+url.QueryEscape("Trip 2")+"&songIndexToRemove=1"); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("step4: %s", rec.Body)
	}
	full = asMap(t, subResp(t, catDo(rt, "/getPlaylist?id="+id))["playlist"])
	if full["name"] != "Trip 2" || len(asList(t, full["entry"])) != 2 {
		t.Fatalf("step4b=%v", full)
	}

	// 5-6. очередь: сохранить и перечитать
	if rec := catDo(rt, fmt.Sprintf(
		"/savePlayQueue?songId=%s&songId=%s&currentIndex=1&position=1000", ids[0], ids[1])); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("step5: %s", rec.Body)
	}
	pq := asMap(t, subResp(t, catDo(rt, "/getPlayQueue"))["playQueue"])
	if pq["current"] != ids[1] || len(asList(t, pq["entry"])) != 2 {
		t.Fatalf("step6=%v", pq)
	}

	// 7. удалить плейлист — очередь живёт своей строкой
	if rec := catDo(rt, "/deletePlaylist?id="+id); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("step7: %s", rec.Body)
	}
	list = asMap(t, subResp(t, catDo(rt, "/getPlaylists"))["playlists"])
	if got := asList(t, list["playlist"]); len(got) != 0 {
		t.Fatalf("step7b=%v", got)
	}
	pq = asMap(t, subResp(t, catDo(rt, "/getPlayQueue"))["playQueue"])
	if pq["current"] != ids[1] {
		t.Fatalf("queue died with playlist: %v", pq)
	}
}
