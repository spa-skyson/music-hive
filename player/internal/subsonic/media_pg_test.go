// Живые PG-тесты медиа-эндпоинтов F3.3 (#25): stream/download/getCoverArt
// (бинарные, реальный media-слой с файлами во временном каталоге), scrobble,
// star/unstar/setRating/getStarred(2), search2/search3, getLyrics(+BySongId),
// типы album-списков starred/frequent/highest/recent и приёмочная цепочка.
//
// Внешний test-пакет: helpers общие с catalog_pg_test.go (тот же пакет).

package subsonic_test

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/media"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// ---------------------------------------------------------------- helpers

// mediaSeed — детерминированные ID свежей базы: artists 1-2, albums 1-3,
// tracks 1-5; реальные аудио-файлы + обложка во временном каталоге.
type mediaSeed struct {
	dir            string
	audio          []byte
	saw, icbyd, mh int64
	tracks         [5]int64
}

const bobPassword = "bob-sub-pass"

func newMediaTestRouter(t *testing.T) (*subsonic.Router, mediaSeed, *db.PGStore) {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.DB.Exec(
		`UPDATE users SET subsonic_md5 = $1 WHERE username = 'owner'`,
		auth.MD5Hex(testPassword)); err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	var s mediaSeed
	s.dir = dir
	s.audio = []byte(strings.Repeat("fake-flac-bytes-", 64)) // ~1KB для Range

	cover := filepath.Join(dir, "cover.png")
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{R: 120, A: 255})
		}
	}
	f, err := os.Create(cover)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	ins := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := store.DB.QueryRow(query+" RETURNING id", args...).Scan(&id); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		return id
	}
	aphex := ins(`INSERT INTO artists(name, name_norm) VALUES ('Aphex Twin', 'aphex twin')`)
	boards := ins(`INSERT INTO artists(name, name_norm) VALUES ('Boards of Canada', 'boards of canada')`)
	s.saw = ins(`INSERT INTO albums(artist_id, title, title_norm, year, created_at)
VALUES ($1, 'Selected Ambient Works', 'selected ambient works', 1993, now() - interval '3 hours')`, aphex)
	s.icbyd = ins(`INSERT INTO albums(artist_id, title, title_norm, year, created_at)
VALUES ($1, 'I Care Because You Do', 'i care because you do', 1995, now() - interval '2 hours')`, aphex)
	s.mh = ins(`INSERT INTO albums(artist_id, title, title_norm, year, created_at)
VALUES ($1, 'Music Has the Right', 'music has the right', 1998, now() - interval '1 hour')`, boards)

	track := func(n int, artist, album string, artistID, albumID int64, artwork string) int64 {
		path := filepath.Join(dir, fmt.Sprintf("%02d.flac", n))
		if err := os.WriteFile(path, s.audio, 0o644); err != nil {
			t.Fatal(err)
		}
		return ins(`INSERT INTO tracks(path, title, artist, album, artist_id, album_artist_id,
	album_id, track_number, year, duration, file_size, bitrate, file_md5, artwork_path)
VALUES ($1,$2,$3,$4,$5,$5,$6,$7,1993,290,1024,900,$8,$9)`,
			path, fmt.Sprintf("Song %d", n), artist, album, artistID, albumID,
			n, fmt.Sprintf("md5-%d", n), artwork)
	}
	s.tracks[0] = track(1, "Aphex Twin", "Selected Ambient Works", aphex, s.saw, cover)
	s.tracks[1] = track(2, "Aphex Twin", "Selected Ambient Works", aphex, s.saw, "")
	s.tracks[2] = track(3, "Aphex Twin", "I Care Because You Do", aphex, s.icbyd, "")
	s.tracks[3] = track(4, "Aphex Twin", "I Care Because You Do", aphex, s.icbyd, "")
	s.tracks[4] = track(5, "Boards of Canada", "Music Has the Right", boards, s.mh, "")

	// lyrics для трека 1 (plain + LRC).
	if _, err := store.DB.Exec(`
INSERT INTO lyrics(track_id, plain_lyrics, synced_lyrics, status)
VALUES ($1, $2, $3, 'ready')`,
		s.tracks[0], "la\nla la", "[00:10.00]sync la\n[00:20.50]sync la la\n"); err != nil {
		t.Fatalf("seed lyrics: %v", err)
	}

	cfg := config.Config{DBPath: filepath.Join(dir, "db", "t.db")}
	rt := &subsonic.Router{
		Users:         store,
		Catalog:       store,
		Library:       store,
		Media:         media.New(cfg, nil, nil),
		Limiter:       auth.NewLoginLimiter(1000, time.Minute),
		ServerVersion: "1.0.0",
		// F3.4 (#26)
		Playlists:     store,
		Queues:        store,
		Bookmarks:     store,
		Radio:         store,
		Accounts:      store,
		Scan:          store,
		PublicBaseURL: "https://mh.example",
	}
	return rt, s, store
}

// ranged — запрос с Range-заголовком.
func ranged(rt *subsonic.Router, path, rangeHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Range", rangeHeader)
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

// bobDo — запрос от второго пользователя (изоляция starred).
func bobDo(t *testing.T, rt *subsonic.Router, store *db.PGStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	uid := pgtest.InsertUser(t, store.DB, "bob", bobPassword, false)
	if _, err := store.DB.Exec(
		`UPDATE users SET subsonic_md5 = $1 WHERE id = $2`, auth.MD5Hex(bobPassword), uid); err != nil {
		t.Fatalf("seed bob creds: %v", err)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return do(rt, "GET", path+sep+"u=bob&p="+bobPassword+"&v=1.16.1&c=test&f=json")
}

// ---------------------------------------------------------------- stream

func TestSubsonicStreamDownload(t *testing.T) {
	rt, s, _ := newMediaTestRouter(t)
	base := "/stream?u=owner&p=" + testPassword + "&v=1.16.1&c=test&f=json"

	rec := do(rt, "GET", base+"&id=1")
	if rec.Code != 200 {
		t.Fatalf("stream status=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/flac" {
		t.Fatalf("content-type=%s", ct)
	}
	if rec.Body.Len() != len(s.audio) {
		t.Fatalf("body len=%d want %d", rec.Body.Len(), len(s.audio))
	}

	// Range → 206 c куском
	rec = ranged(rt, base+"&id=1", "bytes=0-9")
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 10 {
		t.Fatalf("range=%d len=%d", rec.Code, rec.Body.Len())
	}

	// format=raw — оригинал; download — тоже оригинал
	rec = do(rt, "GET", base+"&id=1&format=raw")
	if rec.Code != 200 || rec.Body.Len() != len(s.audio) {
		t.Fatalf("raw=%d len=%d", rec.Code, rec.Body.Len())
	}
	rec = do(rt, "GET", "/download?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json&id=2")
	if rec.Code != 200 || rec.Body.Len() != len(s.audio) {
		t.Fatalf("download=%d len=%d", rec.Code, rec.Body.Len())
	}

	// 70 на несуществующий трек, 10 без id
	wantCode(t, do(rt, "GET", base+"&id=999"), 70)
	wantCode(t, do(rt, "GET", base), 10)
}

// ---------------------------------------------------------------- coverArt

func TestSubsonicGetCoverArt(t *testing.T) {
	rt, _, _ := newMediaTestRouter(t)
	base := "/getCoverArt?u=owner&p=" + testPassword + "&v=1.16.1&c=test&f=json"

	for _, id := range []string{"al-1", "ar-1", "mf-1", "1"} {
		rec := do(rt, "GET", base+"&id="+id)
		if rec.Code != 200 {
			t.Fatalf("%s status=%d", id, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s content-type=%s", id, ct)
		}
	}

	// size → jpeg-превью
	rec := do(rt, "GET", base+"&id=al-1&size=64")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb=%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	// нет artwork / нет сущности / мусорный префикс → 70
	for _, id := range []string{"al-2", "ar-2", "mf-2", "al-999", "xx-1"} {
		wantCode(t, do(rt, "GET", base+"&id="+id), 70)
	}
	wantCode(t, do(rt, "GET", base), 10)
}

// ---------------------------------------------------------------- scrobble

func TestSubsonicScrobble(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)

	ts := time.Date(2024, 5, 1, 10, 30, 0, 0, time.UTC)
	rec := catDo(rt, "/scrobble?id=1&time="+fmt.Sprint(ts.UnixMilli()))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("scrobble: %s", rec.Body)
	}
	var source, action, reason string
	var at time.Time
	if err := store.DB.QueryRow(
		`SELECT source, action, reason, ts FROM listening_history WHERE track_id = 1`).
		Scan(&source, &action, &reason, &at); err != nil {
		t.Fatalf("listening_history: %v", err)
	}
	if source != "subsonic" || action != "track_end" || reason != "completed" {
		t.Fatalf("row=%s/%s/%s", source, action, reason)
	}
	if !at.Equal(ts) {
		t.Fatalf("ts=%v want %v", at, ts)
	}
	// user_track_stats.completed инкрементирован (frequent видит subsonic)
	var completed int
	if err := store.DB.QueryRow(
		`SELECT completed FROM user_track_stats WHERE user_id = 1 AND track_id = 1`).
		Scan(&completed); err != nil || completed != 1 {
		t.Fatalf("completed=%d err=%v", completed, err)
	}

	// submission=false — ok, ничего не пишет
	rec = catDo(rt, "/scrobble?id="+fmt.Sprint(s.tracks[1])+"&submission=false")
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("now-playing: %s", rec.Body)
	}
	var n int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM listening_history WHERE track_id = $1`, s.tracks[1]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("now-playing wrote history: n=%d", n)
	}

	// ошибки
	wantCode(t, catDo(rt, "/scrobble"), 10)
	wantCode(t, catDo(rt, "/scrobble?id=999"), 70)
	wantCode(t, catDo(rt, "/scrobble?id=abc"), 70)
}

// ---------------------------------------------------------------- star

func TestSubsonicStarUnstarAndIsolation(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)

	rec := catDo(rt, fmt.Sprintf("/star?id=%d&albumId=%d&artistId=%d", s.tracks[0], s.saw, 1))
	if subResp(t, rec)["status"] != "ok" {
		t.Fatalf("star: %s", rec.Body)
	}

	sr := subResp(t, catDo(rt, "/getStarred2"))
	starred2 := asMap(t, sr["starred2"])
	if got := asList(t, starred2["song"]); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprint(s.tracks[0]) {
		t.Fatalf("starred2.song=%v", got)
	}
	if got := asList(t, starred2["album"]); len(got) != 1 || asMap(t, got[0])["coverArt"] != fmt.Sprintf("al-%d", s.saw) {
		t.Fatalf("starred2.album=%v", got)
	}
	if got := asList(t, starred2["artist"]); len(got) != 1 {
		t.Fatalf("starred2.artist=%v", got)
	}

	// legacy getStarred: те же данные в Child-форме (ar-/al- идентификаторы)
	sr = subResp(t, catDo(rt, "/getStarred"))
	starred := asMap(t, sr["starred"])
	if got := asList(t, starred["artist"]); len(got) != 1 || asMap(t, got[0])["id"] != "ar-1" {
		t.Fatalf("starred.artist=%v", got)
	}
	if got := asList(t, starred["album"]); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprintf("al-%d", s.saw) {
		t.Fatalf("starred.album=%v", got)
	}

	// изоляция: у bob пусто
	bob := bobDo(t, rt, store, "/getStarred2")
	if got := asList(t, asMap(t, subResp(t, bob)["starred2"])["song"]); len(got) != 0 {
		t.Fatalf("bob sees owner stars: %v", got)
	}

	// unstar → пусто
	if rec := catDo(rt, fmt.Sprintf("/unstar?id=%d&albumId=%d&artistId=%d", s.tracks[0], s.saw, 1)); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("unstar: %s", rec.Body)
	}
	starred2 = asMap(t, subResp(t, catDo(rt, "/getStarred2"))["starred2"])
	for _, block := range []string{"song", "album", "artist"} {
		if got := asList(t, starred2[block]); len(got) != 0 {
			t.Fatalf("after unstar %s=%v", block, got)
		}
	}

	// ошибки
	wantCode(t, catDo(rt, "/star"), 10)
	wantCode(t, catDo(rt, "/star?id=999"), 70)
}

// ---------------------------------------------------------------- rating

func TestSubsonicSetRating(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)
	// kind выводится наличием в tracks/albums/artists: добавляем сущности
	// с id вне диапазона треков, чтобы проверить альбом/артист-ветки.
	// kind выводится наличием в tracks/albums/artists: сдвигаем sequences,
	// чтобы новые альбом/артист имели id вне диапазона треков 1-5.
	var albumID, artistID int64
	if _, err := store.DB.Exec(`SELECT setval('albums_id_seq', 100), setval('artists_id_seq', 200)`); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(
		`INSERT INTO albums(artist_id, title, title_norm) VALUES (1, 'Untitled', 'untitled') RETURNING id`).Scan(&albumID); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(
		`INSERT INTO artists(name, name_norm) VALUES ('New Artist', 'new artist') RETURNING id`).Scan(&artistID); err != nil {
		t.Fatal(err)
	}

	// голый id трека
	if rec := catDo(rt, fmt.Sprintf("/setRating?id=%d&rating=3", s.tracks[0])); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("setRating track: %s", rec.Body)
	}
	var value int
	if err := store.DB.QueryRow(
		`SELECT value FROM user_ratings WHERE user_id = 1 AND kind = 'track' AND track_id = $1`, s.tracks[0]).
		Scan(&value); err != nil || value != 3 {
		t.Fatalf("track rating=%d err=%v", value, err)
	}

	// альбом без треков и артист без пересечений — свои kind
	if rec := catDo(rt, fmt.Sprintf("/setRating?id=%d&rating=5", albumID)); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("setRating album: %s", rec.Body)
	}
	if err := store.DB.QueryRow(
		`SELECT value FROM user_ratings WHERE kind = 'album' AND album_id = $1`, albumID).Scan(&value); err != nil || value != 5 {
		t.Fatalf("album rating=%d err=%v", value, err)
	}
	if rec := catDo(rt, fmt.Sprintf("/setRating?id=%d&rating=2", artistID)); subResp(t, rec)["status"] != "ok" {
		t.Fatalf("setRating artist: %s", rec.Body)
	}
	if err := store.DB.QueryRow(
		`SELECT value FROM user_ratings WHERE kind = 'artist' AND artist_id = $1`, artistID).Scan(&value); err != nil || value != 2 {
		t.Fatalf("artist rating=%d err=%v", value, err)
	}

	// rating=0 удаляет
	catDo(rt, fmt.Sprintf("/setRating?id=%d&rating=0", albumID))
	var n int
	if err := store.DB.QueryRow(
		`SELECT COUNT(*) FROM user_ratings WHERE kind = 'album' AND album_id = $1`, albumID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after zero n=%d", n)
	}

	wantCode(t, catDo(rt, "/setRating?id=1"), 10)            // нет rating
	wantCode(t, catDo(rt, "/setRating?id=1&rating=9"), 10)   // вне 0..5
	wantCode(t, catDo(rt, "/setRating?id=1&rating=x"), 10)   // мусор
	wantCode(t, catDo(rt, "/setRating?id=999&rating=3"), 70) // нет сущности
}

// ---------------------------------------------------------------- search

func TestSubsonicSearch2Search3(t *testing.T) {
	rt, _, _ := newMediaTestRouter(t)

	for _, endpoint := range []string{"search2", "search3"} {
		blockKey := "searchResult2"
		if endpoint == "search3" {
			blockKey = "searchResult3"
		}
		result := asMap(t, subResp(t, catDo(rt, "/"+endpoint+"?query=aphex"))[blockKey])
		artists := asList(t, result["artist"])
		if len(artists) != 1 || asMap(t, artists[0])["name"] != "Aphex Twin" {
			t.Fatalf("%s artists=%v", endpoint, artists)
		}
		albums := asList(t, result["album"])
		if len(albums) != 2 { // оба альбома Aphex Twin
			t.Fatalf("%s albums=%v", endpoint, albums)
		}
		songs := asList(t, result["song"])
		if len(songs) != 4 {
			t.Fatalf("%s songs=%v", endpoint, songs)
		}
	}

	// подстрока по треку: только блок song
	result := asMap(t, subResp(t, catDo(rt, "/search3?query=song%201"))["searchResult3"])
	if got := asList(t, result["song"]); len(got) != 1 || asMap(t, got[0])["title"] != "Song 1" {
		t.Fatalf("song search=%v", got)
	}
	if got := asList(t, result["artist"]); len(got) != 0 {
		t.Fatalf("artists must be empty: %v", got)
	}

	// счётчики и смещения
	result = asMap(t, subResp(t, catDo(rt, "/search3?query=aphex&songCount=2&songOffset=1"))["searchResult3"])
	if got := asList(t, result["song"]); len(got) != 2 {
		t.Fatalf("songCount=2: %v", got)
	}

	wantCode(t, catDo(rt, "/search3"), 10)
	wantCode(t, catDo(rt, "/search2"), 10)
}

// ---------------------------------------------------------------- lyrics

func TestSubsonicLyrics(t *testing.T) {
	rt, s, _ := newMediaTestRouter(t)

	ly := asMap(t, subResp(t, catDo(rt, "/getLyrics?artist=Aphex%20Twin&title=Song%201"))["lyrics"])
	if ly["value"] != "la\nla la" || ly["artist"] != "Aphex Twin" {
		t.Fatalf("lyrics=%v", ly)
	}
	// нет текста — пустой элемент, ok
	if sr := subResp(t, catDo(rt, "/getLyrics?artist=Nobody&title=Nothing")); sr["status"] != "ok" {
		t.Fatalf("missing lyrics: %v", sr)
	}
	wantCode(t, catDo(rt, "/getLyrics?artist=X"), 10)

	list := asMap(t, subResp(t, catDo(rt, fmt.Sprintf("/getLyricsBySongId?id=%d", s.tracks[0])))["lyricsList"])
	entries := asList(t, list["structuredLyrics"])
	if len(entries) != 2 {
		t.Fatalf("entries=%v", entries)
	}
	synced := asMap(t, entries[0])
	if synced["synced"] != true || synced["lang"] != "und" {
		t.Fatalf("synced=%v", synced)
	}
	lines := asList(t, synced["line"])
	if len(lines) != 2 ||
		asMap(t, lines[0])["start"].(float64) != 10000 || asMap(t, lines[0])["value"] != "sync la" ||
		asMap(t, lines[1])["start"].(float64) != 20500 {
		t.Fatalf("synced lines=%v", lines)
	}
	text := asMap(t, entries[1])
	if text["synced"] != false {
		t.Fatalf("text=%v", text)
	}
	if got := asList(t, text["line"]); len(got) != 2 || asMap(t, got[0])["value"] != "la" {
		t.Fatalf("text lines=%v", got)
	}

	// трек без текста и несуществующий id → ok с пустым structuredLyrics
	for _, id := range []string{fmt.Sprint(s.tracks[1]), "999"} {
		list = asMap(t, subResp(t, catDo(rt, "/getLyricsBySongId?id="+id))["lyricsList"])
		if got := asList(t, list["structuredLyrics"]); len(got) != 0 {
			t.Fatalf("no lyrics (%s) entries=%v", id, got)
		}
	}
	wantCode(t, catDo(rt, "/getLyricsBySongId?id=abc"), 70)
}

// ---------------------------------------------------------------- album lists

func TestSubsonicAlbumListsF33(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)

	// подготовка: звёзды + прослушивания + рейтинг владельца
	if _, err := store.DB.Exec(
		`INSERT INTO user_favorites(user_id, kind, album_id) VALUES (1, 'album', $1)`, s.saw); err != nil {
		t.Fatal(err)
	}
	if err := store.SubsonicScrobble(1, s.tracks[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SubsonicScrobble(1, s.tracks[1], time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(
		`INSERT INTO user_ratings(user_id, kind, album_id, value) VALUES (1, 'album', $1, 4)`, s.saw); err != nil {
		t.Fatal(err)
	}

	albumList2 := func(t *testing.T, query string) []any {
		t.Helper()
		return asList(t, asMap(t, subResp(t, catDo(rt, query))["albumList2"])["album"])
	}
	// starred
	if got := albumList2(t, "/getAlbumList2?type=starred"); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprint(s.saw) {
		t.Fatalf("starred=%v", got)
	}
	// frequent: saw (2 completed), у остальных нет прослушиваний
	if got := albumList2(t, "/getAlbumList2?type=frequent"); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprint(s.saw) {
		t.Fatalf("frequent=%v", got)
	}
	// highest
	if got := albumList2(t, "/getAlbumList2?type=highest"); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprint(s.saw) {
		t.Fatalf("highest=%v", got)
	}
	// recent: по max(track.created_at); треки сида созданы «сейчас»,
	// разные секунды не гарантированы — проверяем состав и полноту
	if got := albumList2(t, "/getAlbumList2?type=recent&size=2"); len(got) != 2 {
		t.Fatalf("recent=%v", got)
	}
	// legacy-форма тоже работает
	legacy := asMap(t, subResp(t, catDo(rt, "/getAlbumList?type=starred"))["albumList"])
	if got := asList(t, legacy["album"]); len(got) != 1 || asMap(t, got[0])["id"] != fmt.Sprintf("al-%d", s.saw) {
		t.Fatalf("legacy starred=%v", got)
	}

	// мусорный size/offset не ломает
	if got := albumList2(t, "/getAlbumList2?type=starred&size=abc&offset=-5"); len(got) != 1 {
		t.Fatalf("garbage paging=%v", got)
	}
}

// ---------------------------------------------------------------- chain

// TestSubsonicMediaChain — приёмочная цепочка: getAlbum → stream (200,
// Range 206) → scrobble → star → getStarred2 содержит + frequent.
func TestSubsonicMediaChain(t *testing.T) {
	rt, s, store := newMediaTestRouter(t)

	album := asMap(t, subResp(t, catDo(rt, fmt.Sprintf("/getAlbum?id=%d", s.saw)))["album"])
	songs := asList(t, album["song"])
	if len(songs) != 2 {
		t.Fatalf("album songs=%v", songs)
	}
	songID := asMap(t, songs[0])["id"].(string)

	rec := do(rt, "GET", "/stream?u=owner&p="+testPassword+"&v=1.16.1&c=t&id="+songID+"&format=raw")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/flac" {
		t.Fatalf("stream=%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = ranged(rt, "/stream?u=owner&p="+testPassword+"&v=1.16.1&c=t&id="+songID+"&format=raw", "bytes=16-31")
	if rec.Code != 206 || rec.Body.Len() != 16 {
		t.Fatalf("range=%d len=%d", rec.Code, rec.Body.Len())
	}

	if sr := subResp(t, catDo(rt, "/scrobble?id="+songID)); sr["status"] != "ok" {
		t.Fatalf("scrobble: %v", sr)
	}
	var plays int
	if err := store.DB.QueryRow(`SELECT completed FROM user_track_stats WHERE user_id = 1`).Scan(&plays); err != nil || plays != 1 {
		t.Fatalf("plays=%d err=%v", plays, err)
	}
	if sr := subResp(t, catDo(rt, "/star?id="+songID)); sr["status"] != "ok" {
		t.Fatalf("star: %v", sr)
	}
	starred := asMap(t, subResp(t, catDo(rt, "/getStarred2"))["starred2"])
	found := false
	for _, raw := range asList(t, starred["song"]) {
		if asMap(t, raw)["id"] == songID {
			found = true
		}
	}
	if !found {
		t.Fatalf("song %s not starred: %v", songID, starred["song"])
	}
	// ...и альбом попал в frequent
	freq := asMap(t, subResp(t, catDo(rt, "/getAlbumList2?type=frequent"))["albumList2"])
	if got := asList(t, freq["album"]); len(got) != 1 || asMap(t, got[0])["name"] != "Selected Ambient Works" {
		t.Fatalf("frequent=%v", got)
	}
}
