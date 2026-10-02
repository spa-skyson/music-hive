// Живые PG-тесты каталог-эндпоинтов F3.2 (#24): полный HTTP-стек роутера
// на *db.PGStore (pgtest), сид 2 артиста / 3 альбома / 5 треков / 3 жанра.
// Аутентификация p= (token+salt требует cipher — покрыт юнит-тестами F3.1).
//
// Внешний test-пакет: db импортирует subsonic (контракт Store/Catalog),
// поэтому внутренним тестам db недоступен — цикл.

package subsonic_test

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

const testPassword = "sesame"

// ---------------------------------------------------------------- helpers

func do(rt *subsonic.Router, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func subResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	sr, ok := env["subsonic-response"].(map[string]any)
	if !ok {
		t.Fatalf("no subsonic-response in %v", env)
	}
	return sr
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %v", v)
	}
	return l
}

// wantCode — failed-конверт с кодом протокола.
func wantCode(t *testing.T, rec *httptest.ResponseRecorder, code float64) {
	t.Helper()
	sr := subResp(t, rec)
	if sr["status"] != "failed" {
		t.Fatalf("status=%v body=%s", sr["status"], rec.Body)
	}
	errObj := asMap(t, sr["error"])
	if errObj["code"].(float64) != code {
		t.Fatalf("code=%v want %v: %s", errObj["code"], code, rec.Body)
	}
}

// catDo — аутентифицированный JSON-запрос каталога.
func catDo(rt *subsonic.Router, path string) *httptest.ResponseRecorder {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return do(rt, "GET", path+sep+"u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json")
}

// catSeed — детерминированные ID сиквенса свежей базы: artists 1-2,
// albums 1-3, tracks 1-5.
type catSeed struct {
	aphex, boards int64
	saw           int64 // Selected Ambient Works 85-92, 1993, Electronic
	icbyd         int64 // I Care Because You Do, 1995, Electronic
	mhtrtc        int64 // Music Has the Right to Children, 1998, IDM
	tracks        [5]int64
}

func seedCatalog(t *testing.T, dbh *sql.DB) catSeed {
	t.Helper()
	ins := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := dbh.QueryRow(query+" RETURNING id", args...).Scan(&id); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		return id
	}
	var s catSeed
	s.aphex = ins(`INSERT INTO artists(name, name_norm) VALUES ('Aphex Twin', 'aphex twin')`)
	s.boards = ins(`INSERT INTO artists(name, name_norm) VALUES ('Boards of Canada', 'boards of canada')`)
	s.saw = ins(`INSERT INTO albums(artist_id, title, title_norm, year, genre, created_at)
VALUES ($1, 'Selected Ambient Works 85-92', 'selected ambient works 85-92', 1993, 'Electronic', now() - interval '3 hours')`, s.aphex)
	s.icbyd = ins(`INSERT INTO albums(artist_id, title, title_norm, year, genre, created_at)
VALUES ($1, 'I Care Because You Do', 'i care because you do', 1995, 'Electronic', now() - interval '2 hours')`, s.aphex)
	s.mhtrtc = ins(`INSERT INTO albums(artist_id, title, title_norm, year, genre, created_at)
VALUES ($1, 'Music Has the Right to Children', 'music has the right to children', 1998, 'IDM', now() - interval '1 hour')`, s.boards)

	saw, aphex := s.saw, s.aphex
	s.tracks[0] = ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id, album_id,
	track_number, year, duration, file_size, bitrate, file_md5)
VALUES ('/music/Aphex Twin/Selected Ambient Works 85-92/01 - Xtal.flac', 'Xtal', 'Aphex Twin',
 'Selected Ambient Works 85-92', $1, $1, $2, 1, 1993, 290, 28000000, 900, 'md5-xtal')`, aphex, saw)
	s.tracks[1] = ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id, album_id,
	track_number, year, duration, file_size, bitrate, file_md5)
VALUES ('/music/Aphex Twin/Selected Ambient Works 85-92/02 - Tha.flac', 'Tha', 'Aphex Twin',
 'Selected Ambient Works 85-92', $1, $1, $2, 2, 1993, 545, 52000000, 900, 'md5-tha')`, aphex, saw)
	icbyd := s.icbyd
	s.tracks[2] = ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id, album_id,
	track_number, year, duration, file_size, bitrate, file_md5)
VALUES ('/music/Aphex Twin/I Care Because You Do/01 - Acrid Avid Jam Shred.flac', 'Acrid Avid Jam Shred',
 'Aphex Twin', 'I Care Because You Do', $1, $1, $2, 1, 1995, 470, 45000000, 900, 'md5-acrid')`, aphex, icbyd)
	s.tracks[3] = ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id, album_id,
	track_number, year, duration, file_size, bitrate, file_md5)
VALUES ('/music/Aphex Twin/I Care Because You Do/02 - Ventolin.flac', 'Ventolin',
 'Aphex Twin', 'I Care Because You Do', $1, $1, $2, 2, 1995, 285, 27000000, 900, 'md5-ventolin')`, aphex, icbyd)
	boards, mhtrtc := s.boards, s.mhtrtc
	s.tracks[4] = ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id, album_id,
	track_number, year, duration, file_size, bitrate, file_md5)
VALUES ('/music/Boards of Canada/Music Has the Right to Children/01 - Wildlife Analysis.flac',
 'Wildlife Analysis', 'Boards of Canada', 'Music Has the Right to Children', $1, $1, $2, 1, 1998, 65, 6200000, 900, 'md5-wildlife')`, boards, mhtrtc)

	electronic := ins(`INSERT INTO genres(name) VALUES ('Electronic')`)
	ambient := ins(`INSERT INTO genres(name) VALUES ('Ambient')`)
	idm := ins(`INSERT INTO genres(name) VALUES ('IDM')`)
	for _, tg := range [][2]int64{
		{s.tracks[0], electronic}, {s.tracks[1], electronic}, {s.tracks[2], electronic},
		{s.tracks[3], ambient}, {s.tracks[4], idm},
	} {
		if _, err := dbh.Exec(`INSERT INTO track_genres(track_id, genre_id) VALUES ($1, $2)`, tg[0], tg[1]); err != nil {
			t.Fatalf("seed track_genres: %v", err)
		}
	}
	return s
}

// newCatalogTestRouter — роутер на живом PG (пользователь owner с
// subsonic-паролем testPassword по p=-схеме).
func newCatalogTestRouter(t *testing.T) (*subsonic.Router, catSeed) {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// OpenPG бутстрапит владельца 'owner' — задаём ему subsonic-пароль
	// (p=-схема аутентификации; token+salt покрыт юнит-тестами F3.1).
	if _, err := store.DB.Exec(
		`UPDATE users SET subsonic_md5 = $1 WHERE username = 'owner'`,
		auth.MD5Hex(testPassword)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seed := seedCatalog(t, store.DB)
	return &subsonic.Router{
		Users:         store,
		Catalog:       store,
		Library:       store, // F3.3 (#25): starred/frequent-типы списков
		Limiter:       auth.NewLoginLimiter(1000, time.Minute),
		ServerVersion: "1.0.0",
	}, seed
}

// ---------------------------------------------------------------- getMusicFolders

func TestCatalogGetMusicFolders(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getMusicFolders")
	folders := asMap(t, subResp(t, rec)["musicFolders"])
	list := asList(t, folders["musicFolder"])
	if len(list) != 1 {
		t.Fatalf("folders=%v", list)
	}
	f := asMap(t, list[0])
	if f["id"] != "1" || f["name"] != "music" {
		t.Fatalf("folder=%v", f)
	}
	// XML-форма и .view-написание.
	rec = do(rt, "GET", "/getMusicFolders.view?u=owner&p="+testPassword+"&v=1.16.1&c=t")
	if !strings.Contains(rec.Body.String(), `<musicFolder id="1" name="music"></musicFolder>`) {
		t.Fatalf("xml: %s", rec.Body)
	}
}

// ---------------------------------------------------------------- getArtists

func TestCatalogGetArtists(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getArtists")
	artists := asMap(t, subResp(t, rec)["artists"])
	index := asList(t, artists["index"])
	if len(index) != 2 {
		t.Fatalf("index=%v", index)
	}
	a := asMap(t, index[0])
	if a["name"] != "A" {
		t.Fatalf("first block=%v", a["name"])
	}
	at := asMap(t, asList(t, a["artist"])[0])
	if at["id"] != "1" || at["name"] != "Aphex Twin" || at["albumCount"].(float64) != 2 ||
		at["coverArt"] != "ar-1" {
		t.Fatalf("artist A=%v", at)
	}
	b := asMap(t, index[1])
	bat := asMap(t, asList(t, b["artist"])[0])
	if b["name"] != "B" || bat["name"] != "Boards of Canada" || bat["albumCount"].(float64) != 1 ||
		bat["coverArt"] != "ar-2" {
		t.Fatalf("artist B=%v", b)
	}
}

// TestCatalogGetArtistsXMLGolden — снапшот XML (IDs детерминированы свежей
// базой: artists 1-2).
func TestCatalogGetArtistsXMLGolden(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := do(rt, "GET", "/getArtists?u=owner&p="+testPassword+"&v=1.16.1&c=test")
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("content-type=%s", ct)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "getArtists.xml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if rec.Body.String() != string(want) {
		t.Fatalf("getArtists XML mismatch:\n got: %s\nwant: %s", rec.Body, want)
	}
}

// ---------------------------------------------------------------- getArtist

func TestCatalogGetArtist(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getArtist?id=1")
	artist := asMap(t, subResp(t, rec)["artist"])
	if artist["id"] != "1" || artist["name"] != "Aphex Twin" ||
		artist["albumCount"].(float64) != 2 || artist["coverArt"] != "ar-1" {
		t.Fatalf("artist=%v", artist)
	}
	albums := asList(t, artist["album"])
	if len(albums) != 2 {
		t.Fatalf("albums=%v", albums)
	}
	first := asMap(t, albums[0]) // по году: 1993 раньше 1995
	if first["id"] != "1" || first["name"] != "Selected Ambient Works 85-92" ||
		first["artist"] != "Aphex Twin" || first["artistId"] != "1" ||
		first["year"].(float64) != 1993 || first["songCount"].(float64) != 2 ||
		first["duration"].(float64) != 835 || first["coverArt"] != "al-1" ||
		first["genre"] != "Electronic" || first["created"] == "" {
		t.Fatalf("album[0]=%v", first)
	}
	if asMap(t, albums[1])["name"] != "I Care Because You Do" {
		t.Fatalf("album[1]=%v", albums[1])
	}

	// 70: неизвестный, нечисловой (префиксный id — не entity-id), ноль.
	for _, q := range []string{"id=999", "id=ar-1", "id=0", "id=abc"} {
		wantCode(t, catDo(rt, "/getArtist?"+q), 70)
	}
	// 10: параметр отсутствует.
	wantCode(t, catDo(rt, "/getArtist"), 10)
}

// ---------------------------------------------------------------- getAlbum

func TestCatalogGetAlbum(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getAlbum?id=1")
	album := asMap(t, subResp(t, rec)["album"])
	if album["id"] != "1" || album["name"] != "Selected Ambient Works 85-92" ||
		album["songCount"].(float64) != 2 || album["duration"].(float64) != 835 ||
		album["coverArt"] != "al-1" || album["artist"] != "Aphex Twin" {
		t.Fatalf("album=%v", album)
	}
	songs := asList(t, album["song"])
	if len(songs) != 2 {
		t.Fatalf("songs=%v", songs)
	}
	song := asMap(t, songs[0])
	if song["id"] != "1" || song["parent"] != "1" || song["isDir"] != false ||
		song["title"] != "Xtal" || song["artist"] != "Aphex Twin" ||
		song["track"].(float64) != 1 || song["year"].(float64) != 1993 ||
		song["duration"].(float64) != 290 || song["size"].(float64) != 28000000 ||
		song["suffix"] != "flac" || song["contentType"] != "audio/flac" ||
		song["coverArt"] != "mf-1" || song["albumId"] != "1" || song["artistId"] != "1" ||
		song["bitRate"].(float64) != 900 || song["type"] != "music" {
		t.Fatalf("song[0]=%v", song)
	}
	if asMap(t, songs[1])["id"] != "2" { // порядок по track_number
		t.Fatalf("song[1]=%v", songs[1])
	}

	// XML-форма: альбом с песнями.
	rec = do(rt, "GET", "/getAlbum?id=1&u=owner&p="+testPassword+"&v=1.16.1&c=t")
	body := rec.Body.String()
	if !strings.Contains(body, `<song id="1" parent="1" isDir="false"`) ||
		!strings.Contains(body, `coverArt="mf-1"`) {
		t.Fatalf("xml: %s", body)
	}

	wantCode(t, catDo(rt, "/getAlbum?id=999"), 70)
	wantCode(t, catDo(rt, "/getAlbum?id=al-1"), 70)
	wantCode(t, catDo(rt, "/getAlbum"), 10)
}

// ---------------------------------------------------------------- getSong

func TestCatalogGetSong(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getSong?id=5")
	song := asMap(t, subResp(t, rec)["song"])
	if song["id"] != "5" || song["parent"] != "3" || song["title"] != "Wildlife Analysis" ||
		song["artist"] != "Boards of Canada" || song["isDir"] != false ||
		song["suffix"] != "flac" || song["coverArt"] != "mf-5" {
		t.Fatalf("song=%v", song)
	}
	rec = do(rt, "GET", "/getSong?id=5&u=owner&p="+testPassword+"&v=1.16.1&c=t")
	if !strings.Contains(rec.Body.String(), `<song id="5"`) {
		t.Fatalf("xml: %s", rec.Body)
	}
	wantCode(t, catDo(rt, "/getSong?id=999"), 70)
	wantCode(t, catDo(rt, "/getSong?id=xx"), 70)
	wantCode(t, catDo(rt, "/getSong"), 10)
}

// ---------------------------------------------------------------- legacy-дерево

func TestCatalogGetIndexes(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getIndexes")
	indexes := asMap(t, subResp(t, rec)["indexes"])
	index := asList(t, indexes["index"])
	if len(index) != 2 {
		t.Fatalf("index=%v", index)
	}
	a := asMap(t, asList(t, asMap(t, index[0])["artist"])[0])
	if a["id"] != "ar-1" || a["name"] != "Aphex Twin" {
		t.Fatalf("index artist=%v", a)
	}
	rec = do(rt, "GET", "/getIndexes?u=owner&p="+testPassword+"&v=1.16.1&c=t")
	if !strings.Contains(rec.Body.String(), `<artist id="ar-1" name="Aphex Twin">`) {
		t.Fatalf("xml: %s", rec.Body)
	}
}

func TestCatalogGetMusicDirectory(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)

	// ar-<artistID> → альбомы-псевдо-директории.
	rec := catDo(rt, "/getMusicDirectory?id=ar-1")
	dir := asMap(t, subResp(t, rec)["directory"])
	if dir["id"] != "ar-1" || dir["name"] != "Aphex Twin" {
		t.Fatalf("dir=%v", dir)
	}
	children := asList(t, dir["child"])
	if len(children) != 2 {
		t.Fatalf("children=%v", children)
	}
	c := asMap(t, children[0])
	if c["id"] != "al-1" || c["parent"] != "ar-1" || c["isDir"] != true ||
		c["title"] != "Selected Ambient Works 85-92" || c["year"].(float64) != 1993 ||
		c["coverArt"] != "al-1" {
		t.Fatalf("album child=%v", c)
	}

	// al-<albumID> → треки-чилды с parent=al-<id>.
	rec = catDo(rt, "/getMusicDirectory?id=al-1")
	dir = asMap(t, subResp(t, rec)["directory"])
	if dir["id"] != "al-1" || dir["name"] != "Selected Ambient Works 85-92" {
		t.Fatalf("dir=%v", dir)
	}
	songs := asList(t, dir["child"])
	if len(songs) != 2 {
		t.Fatalf("songs=%v", songs)
	}
	s := asMap(t, songs[0])
	if s["id"] != "1" || s["parent"] != "al-1" || s["isDir"] != false || s["title"] != "Xtal" {
		t.Fatalf("song child=%v", s)
	}

	// 70: неизвестные/некорректные dir-ID; 10: без id.
	for _, q := range []string{
		"id=ar-999", "id=al-999", "id=1", "id=xx-1", "id=ar-x", "id=al-", "id=mf-1",
	} {
		wantCode(t, catDo(rt, "/getMusicDirectory?"+q), 70)
	}
	wantCode(t, catDo(rt, "/getMusicDirectory"), 10)
}

// ---------------------------------------------------------------- жанры

func TestCatalogGetGenres(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getGenres")
	genres := asMap(t, subResp(t, rec)["genres"])
	list := asList(t, genres["genre"])
	if len(list) != 3 {
		t.Fatalf("genres=%v", list)
	}
	ambient := asMap(t, list[0]) // сортировка по имени
	if ambient["value"] != "Ambient" || ambient["songCount"].(float64) != 1 ||
		ambient["albumCount"].(float64) != 0 {
		t.Fatalf("ambient=%v", ambient)
	}
	electronic := asMap(t, list[1])
	if electronic["value"] != "Electronic" || electronic["songCount"].(float64) != 3 ||
		electronic["albumCount"].(float64) != 2 {
		t.Fatalf("electronic=%v", electronic)
	}
	idm := asMap(t, list[2])
	if idm["value"] != "IDM" || idm["albumCount"].(float64) != 1 {
		t.Fatalf("idm=%v", idm)
	}
	rec = do(rt, "GET", "/getGenres?u=owner&p="+testPassword+"&v=1.16.1&c=t")
	if !strings.Contains(rec.Body.String(),
		`<genre songCount="3" albumCount="2">Electronic</genre>`) {
		t.Fatalf("xml: %s", rec.Body)
	}
}

// ---------------------------------------------------------------- списки альбомов

func TestCatalogGetAlbumList2(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	names := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		wrapper := asMap(t, subResp(t, rec)["albumList2"])
		var out []string
		for _, a := range asList(t, wrapper["album"]) {
			out = append(out, asMap(t, a)["name"].(string))
		}
		return out
	}
	cases := []struct {
		q    string
		want []string
	}{
		{"type=alphabeticalByName", []string{"I Care Because You Do", "Music Has the Right to Children", "Selected Ambient Works 85-92"}},
		{"type=alphabeticalArtist", []string{"I Care Because You Do", "Selected Ambient Works 85-92", "Music Has the Right to Children"}},
		{"type=newest", []string{"Music Has the Right to Children", "I Care Because You Do", "Selected Ambient Works 85-92"}},
		{"type=byYear&fromYear=1990&toYear=1996", []string{"Selected Ambient Works 85-92", "I Care Because You Do"}},
		{"type=byYear&fromYear=1996&toYear=1990", []string{"I Care Because You Do", "Selected Ambient Works 85-92"}},
		{"type=byGenre&genre=Electronic", []string{"Selected Ambient Works 85-92", "I Care Because You Do"}},
		{"type=alphabeticalByName&size=1&offset=1", []string{"Music Has the Right to Children"}},
		{"type=alphabeticalByName&size=9999", []string{"I Care Because You Do", "Music Has the Right to Children", "Selected Ambient Works 85-92"}},
	}
	for _, tc := range cases {
		if got := names(catDo(rt, "/getAlbumList2?"+tc.q)); !equalStrings(got, tc.want) {
			t.Fatalf("q=%s: got %v want %v", tc.q, got, tc.want)
		}
	}

	// random: длина и уникальность.
	got := names(catDo(rt, "/getAlbumList2?type=random&size=2"))
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("random=%v", got)
	}

	// Пользовательских данных нет: starred/frequent/highest — пустые списки
	// (массив, не null), не ошибка; recent — реальный источник с F3.3:
	// альбомы по свежести треков.
	for _, typ := range []string{"frequent", "highest", "starred"} {
		rec := catDo(rt, "/getAlbumList2?type="+typ)
		if subResp(t, rec)["status"] != "ok" {
			t.Fatalf("%s: %s", typ, rec.Body)
		}
		if list := names(rec); len(list) != 0 {
			t.Fatalf("%s: %v", typ, list)
		}
	}
	if got := names(catDo(rt, "/getAlbumList2?type=recent")); len(got) != 3 ||
		got[0] != "Music Has the Right to Children" {
		t.Fatalf("recent=%v", got)
	}

	// Ошибки параметров.
	for _, q := range []string{
		"", "type=weird", "type=byGenre", "type=byYear&fromYear=1990", "type=byYear",
	} {
		wantCode(t, catDo(rt, "/getAlbumList2?"+q), 10)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCatalogGetAlbumListLegacy(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getAlbumList?type=alphabeticalByName")
	wrapper := asMap(t, subResp(t, rec)["albumList"])
	list := asList(t, wrapper["album"])
	if len(list) != 3 {
		t.Fatalf("albums=%v", list)
	}
	first := asMap(t, list[0])
	if first["id"] != "al-2" || first["parent"] != "ar-1" || first["isDir"] != true ||
		first["title"] != "I Care Because You Do" || first["artist"] != "Aphex Twin" {
		t.Fatalf("legacy album=%v", first)
	}
	// starred без звёзд у пользователя — пустой список.
	rec = catDo(rt, "/getAlbumList?type=starred")
	if list := asList(t, asMap(t, subResp(t, rec)["albumList"])["album"]); len(list) != 0 {
		t.Fatalf("starred empty=%v", list)
	}
}

// ---------------------------------------------------------------- подборки

func TestCatalogGetRandomSongs(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	rec := catDo(rt, "/getRandomSongs?size=2")
	songs := asList(t, asMap(t, subResp(t, rec)["randomSongs"])["song"])
	if len(songs) != 2 || asMap(t, songs[0])["id"] == asMap(t, songs[1])["id"] {
		t.Fatalf("random songs=%v", songs)
	}
	// size по умолчанию — весь каталог из 5.
	rec = catDo(rt, "/getRandomSongs")
	if songs = asList(t, asMap(t, subResp(t, rec)["randomSongs"])["song"]); len(songs) != 5 {
		t.Fatalf("default size=%d", len(songs))
	}
}

func TestCatalogGetSongsByGenre(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)
	ids := func(rec *httptest.ResponseRecorder) []string {
		var out []string
		for _, s := range asList(t, asMap(t, subResp(t, rec)["songsByGenre"])["song"]) {
			out = append(out, asMap(t, s)["id"].(string))
		}
		return out
	}
	if got := ids(catDo(rt, "/getSongsByGenre?genre=Electronic")); !equalStrings(got, []string{"1", "2", "3"}) {
		t.Fatalf("electronic=%v", got)
	}
	if got := ids(catDo(rt, "/getSongsByGenre?genre=Electronic&count=2")); !equalStrings(got, []string{"1", "2"}) {
		t.Fatalf("count=2: %v", got)
	}
	if got := ids(catDo(rt, "/getSongsByGenre?genre=Electronic&count=2&offset=1")); !equalStrings(got, []string{"2", "3"}) {
		t.Fatalf("offset=1: %v", got)
	}
	if got := ids(catDo(rt, "/getSongsByGenre?genre=Ambient")); !equalStrings(got, []string{"4"}) {
		t.Fatalf("ambient=%v", got)
	}
	if got := ids(catDo(rt, "/getSongsByGenre?genre=Nosuch")); len(got) != 0 {
		t.Fatalf("unknown genre=%v", got)
	}
	wantCode(t, catDo(rt, "/getSongsByGenre"), 10)
}

// ---------------------------------------------------------------- цепочка

// TestCatalogBrowseChain — DoD #24: getArtists → getArtist → getAlbum →
// getSong на строковых id из ответов (никаких префиксов в entity-id).
func TestCatalogBrowseChain(t *testing.T) {
	rt, _ := newCatalogTestRouter(t)

	index := asList(t, asMap(t, subResp(t, catDo(rt, "/getArtists"))["artists"])["index"])
	artist := asMap(t, asList(t, asMap(t, index[0])["artist"])[0])
	artistID := artist["id"].(string)

	artistResp := asMap(t, subResp(t, catDo(rt, "/getArtist?id="+artistID))["artist"])
	album := asMap(t, asList(t, artistResp["album"])[0])
	albumID := album["id"].(string)

	albumResp := asMap(t, subResp(t, catDo(rt, "/getAlbum?id="+albumID))["album"])
	song := asMap(t, asList(t, albumResp["song"])[0])
	songID := song["id"].(string)

	songResp := asMap(t, subResp(t, catDo(rt, "/getSong?id="+songID))["song"])
	if songResp["title"] != song["title"] || songResp["id"] != songID ||
		songResp["parent"] != albumID {
		t.Fatalf("chain broken: %v vs %v", songResp, song)
	}
	// legacy-ветка: индекс → директория артиста → директория альбома → тот же трек.
	idxArtist := asMap(t, asList(t, asMap(t, asList(t,
		asMap(t, subResp(t, catDo(rt, "/getIndexes"))["indexes"])["index"])[0])["artist"])[0])
	dirArtist := asMap(t, subResp(t, catDo(rt, "/getMusicDirectory?id="+idxArtist["id"].(string)))["directory"])
	dirAlbum := asMap(t, asList(t, dirArtist["child"])[0])
	dirSongs := asList(t, asMap(t, subResp(t,
		catDo(rt, "/getMusicDirectory?id="+dirAlbum["id"].(string)))["directory"])["child"])
	if asMap(t, dirSongs[0])["id"] != songID {
		t.Fatalf("legacy tree id mismatch: %v vs %s", dirSongs[0], songID)
	}
}
