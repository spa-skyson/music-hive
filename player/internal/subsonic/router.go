// Пакет subsonic — слой Subsonic /rest/* API (F3.1, GitLab #23): конверт
// subsonic-response (двойной рендер JSON/XML/jsonp одним DTO), собственная
// аутентификация протокола (u+t+s / u+p), системные эндпоинты. Роутинг и
// middleware частично производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/api.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// validJSIdentifier — анти-XSS фильтр jsonp-callback (regexp из Navidrome).
var validJSIdentifier = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$.]*$`)

// Store — подокно db-хранилища для subsonic-аутентификации
// (реализация — *db.PGStore).
type Store interface {
	AuthSubsonicCredentials(username string) (auth.SubsonicCredentials, bool, error)
	AuthSetSubsonicPassword(userID int64, md5hex, enc string) error
}

// Catalog — read-only окно каталога для browse/list/search-эндпоинтов
// (F3.2, GitLab #24; расширено в F3.3, #25): artists/albums/tracks/genres/
// lyrics без user_id (каталог общий). Реализация — *db.PGStore; контрактные
// типы живут здесь, db их возвращает (паттерн F3.1: auth.SubsonicCredentials
// в пакете auth).
type Catalog interface {
	SubsonicArtists() ([]ArtistRow, error)
	SubsonicArtist(id int64) (ArtistRow, bool, error)
	SubsonicArtistAlbums(artistID int64) ([]AlbumRow, error)
	SubsonicAlbum(id int64) (AlbumRow, bool, error)
	SubsonicAlbumSongs(albumID int64) ([]SongRow, error)
	SubsonicSong(id int64) (SongRow, bool, error)
	SubsonicAlbumList(q AlbumListQuery) ([]AlbumRow, error)
	SubsonicRandomSongs(size int) ([]SongRow, error)
	SubsonicSongsByGenre(genre string, count, offset int) ([]SongRow, error)
	SubsonicGenres() ([]GenreRow, error)

	// Поиск и списки F3.3 (#25).
	SubsonicSearchArtists(query string, count, offset int) ([]ArtistRow, error)
	SubsonicSearchAlbums(query string, count, offset int) ([]AlbumRow, error)
	SubsonicSearchSongs(query string, count, offset int) ([]SongRow, error)
	SubsonicRecentAlbums(size, offset int) ([]AlbumRow, error)
	SubsonicHighestAlbums(size, offset int) ([]AlbumRow, error)

	// Обложки: resolve сущности → трек с artwork_path (getCoverArt).
	SubsonicCoverTrack(kind string, id int64) (trackID int64, artworkPath string, found bool, err error)

	// Тексты (getLyrics / getLyricsBySongId).
	SubsonicLyricsForSong(artist, title string) (LyricsRow, bool, error)
	SubsonicLyricsForTrack(trackID int64) (LyricsRow, bool, error)
}

// Library — user-scoped данные медиа-аннотаций (F3.3, GitLab #25):
// scrobble → listening_history (+ user_track_stats), star/unstar →
// user_favorites, setRating → user_ratings, starred/frequent-списки.
// Реализация — *db.PGStore.
type Library interface {
	SubsonicScrobble(userID, trackID int64, at time.Time) error
	SubsonicFavoriteSet(userID int64, kind string, id int64, starred bool) error
	SubsonicRatingSet(userID int64, kind string, id int64, rating int) error
	SubsonicStarredSongs(userID int64) ([]SongRow, error)
	SubsonicStarredAlbums(userID int64) ([]AlbumRow, error)
	SubsonicStarredArtists(userID int64) ([]ArtistRow, error)
	SubsonicFrequentAlbums(userID int64, size, offset int) ([]AlbumRow, error)
}

// Media — отдача аудио и обложек (реализация — *media.Service; стриминг и
// транскод переиспользуются из /api/stream, без дублирования).
type Media interface {
	ServeMobile(w http.ResponseWriter, r *http.Request, id int64, srcPath string)
	ServeOriginal(w http.ResponseWriter, r *http.Request, path string)
	ServeArtworkFile(w http.ResponseWriter, r *http.Request, trackID int64, path string, size int) bool
}

// ArtistRow — артист с числом альбомов (index-блоки getArtists/getIndexes).
type ArtistRow struct {
	ID         int64
	Name       string
	NameNorm   string
	AlbumCount int
}

// AlbumRow — альбом с агрегатами по активным трекам.
type AlbumRow struct {
	ID        int64
	ArtistID  int64
	Artist    string
	Title     string
	Year      int
	Genre     string
	SongCount int
	Duration  int
	CreatedAt time.Time
}

// SongRow — трек для child-элементов (Path нужен для suffix/contentType).
type SongRow struct {
	ID         int64
	AlbumID    int64
	ArtistID   int64
	Title      string
	Artist     string
	Album      string
	Track      int
	Year       int
	Duration   int
	Size       int64
	Bitrate    int
	DiscNumber int
	Path       string
	CreatedAt  time.Time
}

// GenreRow — жанр со счётчиками треков/альбомов.
type GenreRow struct {
	Name       string
	SongCount  int
	AlbumCount int
}

// LyricsRow — текст песни из lyrics-таблицы (plain + LRC),artist/title —
// denorm трека для display-полей OpenSubsonic.
type LyricsRow struct {
	Artist string
	Title  string
	Plain  string
	Synced string
}

// AlbumListQuery — параметры getAlbumList/getAlbumList2. Типы без SQL
// (recent/frequent/highest/starred) отсеиваются хендлером до Catalog.
type AlbumListQuery struct {
	Type     string
	Genre    string
	FromYear int
	ToYear   int
	Size     int
	Offset   int
}

// Router — /rest/*-маршруты. Монтируется в api.Server под префиксом /rest/
// (без общего /api-gate: своя аутентификация по спецификации Subsonic).
type Router struct {
	Users         Store
	Catalog       Catalog       // nil допустим: каталог-эндпоинты зовёт только PG-режим
	Library       Library       // nil допустим: user-scoped данные F3.3 (#25)
	Media         Media         // nil допустим: бинарные stream/getCoverArt (#25)
	Playlists     PlaylistStore // nil допустим: плейлисты F3.4 (#26)
	Queues        QueueStore    // nil допустим: play-queue/getNowPlaying F3.4 (#26)
	Bookmarks     BookmarkStore // nil допустим: закладки F3.4 (#26)
	Radio         RadioStore    // nil допустим: интернет-радио F3.4 (#26)
	Accounts      UserAccounts  // nil допустим: getUser/getUsers F3.4 (#26)
	Scan          ScanStore     // nil допустим: getScanStatus/startScan F3.4 (#26)
	Similar       SimilarFunc   // nil допустим: getSimilarSongs(2) F3.4 (#26)
	PublicBaseURL string        // база streamUrl радио-станций (иначе из запроса)
	Cipher        *auth.SubsonicCipher
	Limiter       *auth.LoginLimiter
	Disabled      bool   // MUSIC_HIVE_SUBSONIC_AUTH=0 → ошибка 50 с пояснением
	ServerVersion string // поле serverVersion конверта (сборка player)
}

// handler — обычный эндпоинт: возвращает конверт; ошибка → sendError.
type handler func(*http.Request) (*Subsonic, error)

// codedError — ошибка с кодом протокола (см. NewError); httpStatus≠0 —
// нестандартный HTTP-код до тела (501 у not-implemented-заглушек).
type codedError struct {
	code       int32
	msg        string
	httpStatus int
}

func (e *codedError) Error() string { return e.msg }

// NewError — ошибка субсоник-эндпоинта с кодом протокола; пустой msg —
// стандартное сообщение кода. Для хендлеров F3.2+.
func NewError(code int32, msg string) error {
	if msg == "" {
		msg = ErrorMsg(code)
	}
	return &codedError{code: code, msg: msg}
}

// notImplemented — эндпоинт зарегистрирован, но ждёт своей фазы (#25):
// конверт code 0 + HTTP 501, клиенты не ловят 404.
func notImplemented(what string) error {
	return &codedError{
		code:       ErrorGeneric,
		msg:        what + " is not implemented yet",
		httpStatus: http.StatusNotImplemented,
	}
}

// NewResponse — конверт успешного ответа.
func (rt *Router) NewResponse() *Subsonic {
	return &Subsonic{
		Status:        StatusOK,
		Version:       Version,
		Type:          ServerType,
		ServerVersion: rt.ServerVersion,
		OpenSubsonic:  true,
	}
}

func (rt *Router) errorResponse(code int32, msg string) *Subsonic {
	resp := rt.NewResponse()
	resp.Status = StatusFailed
	resp.Error = &Error{Code: code, Message: msg}
	return resp
}

// Handler — маршруты /rest/*. Каждый путь регистрируется дважды: /ping и
// /ping.view (старые клиенты ходят с суффиксом .view).
func (rt *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	// Публичные (без auth) — getOpenSubsonicExtensions по OpenSubsonic.
	rt.add(mux, "getOpenSubsonicExtensions", rt.getOpenSubsonicExtensions, true)

	// Системные.
	rt.add(mux, "ping", rt.ping, false)
	rt.add(mux, "getLicense", rt.getLicense, false)

	// Каталог (F3.2, GitLab #24): ID3-browse + legacy-дерево.
	rt.add(mux, "getMusicFolders", rt.getMusicFolders, false)
	rt.add(mux, "getArtists", rt.getArtists, false)
	rt.add(mux, "getArtist", rt.getArtist, false)
	rt.add(mux, "getAlbum", rt.getAlbum, false)
	rt.add(mux, "getSong", rt.getSong, false)
	rt.add(mux, "getIndexes", rt.getIndexes, false)
	rt.add(mux, "getMusicDirectory", rt.getMusicDirectory, false)
	rt.add(mux, "getGenres", rt.getGenres, false)
	rt.add(mux, "getAlbumList", rt.getAlbumList, false)
	rt.add(mux, "getAlbumList2", rt.getAlbumList2, false)
	rt.add(mux, "getRandomSongs", rt.getRandomSongs, false)
	rt.add(mux, "getSongsByGenre", rt.getSongsByGenre, false)

	// Поиск (F3.3, #25): search2/search3; legacy search — 501 (старейшая
	// форма параметров, живые клиенты не используют).
	rt.add(mux, "search", rt.search, false)
	rt.add(mux, "search2", rt.search2, false)
	rt.add(mux, "search3", rt.search3, false)

	// Медиа-аннотации (F3.3, #25).
	rt.add(mux, "scrobble", rt.scrobble, false)
	rt.add(mux, "star", rt.star, false)
	rt.add(mux, "unstar", rt.unstar, false)
	rt.add(mux, "setRating", rt.setRating, false)
	rt.add(mux, "getStarred", rt.getStarred, false)
	rt.add(mux, "getStarred2", rt.getStarred2, false)
	rt.add(mux, "getLyrics", rt.getLyrics, false)
	rt.add(mux, "getLyricsBySongId", rt.getLyricsBySongId, false)

	// Бинарные (тело — не конверт): аудио и обложки.
	rt.addBinary(mux, "stream", rt.stream)
	rt.addBinary(mux, "download", rt.download)
	rt.addBinary(mux, "getCoverArt", rt.getCoverArt)

	// Плейлисты и play-queue (F3.4, GitLab #26).
	rt.add(mux, "getPlaylists", rt.getPlaylists, false)
	rt.add(mux, "getPlaylist", rt.getPlaylist, false)
	rt.add(mux, "createPlaylist", rt.createPlaylist, false)
	rt.add(mux, "updatePlaylist", rt.updatePlaylist, false)
	rt.add(mux, "deletePlaylist", rt.deletePlaylist, false)
	rt.add(mux, "getPlayQueue", rt.getPlayQueue, false)
	rt.add(mux, "savePlayQueue", rt.savePlayQueue, false)

	// Закладки, интернет-радио, пользователи, скан, похожие (F3.4, #26).
	rt.add(mux, "getBookmarks", rt.getBookmarks, false)
	rt.add(mux, "createBookmark", rt.createBookmark, false)
	rt.add(mux, "deleteBookmark", rt.deleteBookmark, false)
	rt.add(mux, "getInternetRadioStations", rt.getInternetRadioStations, false)
	rt.add(mux, "createInternetRadioStation", rt.createInternetRadioStation, false)
	rt.add(mux, "updateInternetRadioStation", rt.updateInternetRadioStation, false)
	rt.add(mux, "deleteInternetRadioStation", rt.deleteInternetRadioStation, false)
	rt.add(mux, "getUser", rt.getUser, false)
	rt.add(mux, "getUsers", rt.getUsers, false)
	rt.add(mux, "getScanStatus", rt.getScanStatus, false)
	rt.add(mux, "startScan", rt.startScan, false)
	rt.add(mux, "getNowPlaying", rt.getNowPlaying, false)
	rt.add(mux, "getSimilarSongs", rt.getSimilarSongs, false)
	rt.add(mux, "getSimilarSongs2", rt.getSimilarSongs2, false)
	rt.add(mux, "getTopSongs", rt.getTopSongs, false) // 501: нет данных популярности
	rt.add(mux, "getAvatar", rt.getAvatar, false)     // 70: аватаров нет

	return rt.postFormToQueryParams(mux)
}

// add регистрирует handler под двумя написаниями пути. public=true — без
// auth-middleware (getOpenSubsonicExtensions); иначе: обязательные
// параметры → аутентификация → хендлер.
func (rt *Router) add(mux *http.ServeMux, path string, h handler, public bool) {
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		if !public {
			if !rt.checkRequiredParameters(w, r) {
				return
			}
			if !rt.authenticate(w, r) {
				return
			}
		}
		resp, err := h(r)
		if err != nil {
			rt.sendError(w, r, err)
			return
		}
		if resp != nil {
			rt.sendResponse(w, r, resp, 0)
		}
	}
	mux.HandleFunc("/"+path, wrapped)
	mux.HandleFunc("/"+path+".view", wrapped)
}

// binaryHandler — эндпоинт, пишущий тело сам (stream/download/getCoverArt):
// аудио и картинки не заворачиваются в subsonic-конверт. Ошибки до первой
// записи тела — обычный sendError.
type binaryHandler func(http.ResponseWriter, *http.Request)

// addBinary — как add, но хендлер пишет ответ сам (никакого sendResponse).
func (rt *Router) addBinary(mux *http.ServeMux, path string, h binaryHandler) {
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		if !rt.checkRequiredParameters(w, r) {
			return
		}
		if !rt.authenticate(w, r) {
			return
		}
		h(w, r)
	}
	mux.HandleFunc("/"+path, wrapped)
	mux.HandleFunc("/"+path+".view", wrapped)
}

// sendError — ошибка → конверт failed; codedError несёт код протокола,
// прочие ошибки → code 0 (как mapToSubsonicError у Navidrome).
func (rt *Router) sendError(w http.ResponseWriter, r *http.Request, err error) {
	var sub *codedError
	if !errors.As(err, &sub) {
		sub = &codedError{code: ErrorGeneric, msg: fmt.Sprintf("Internal Server Error: %s", err)}
	}
	rt.sendResponse(w, r, rt.errorResponse(sub.code, sub.msg), sub.httpStatus)
}

// sendResponse пишет конверт в формате f=json|jsonp|xml (по умолчанию XML,
// как в спецификации). status≠0 — нестандартный HTTP-код до тела.
func (rt *Router) sendResponse(w http.ResponseWriter, r *http.Request, payload *Subsonic, status int) {
	f := r.URL.Query().Get("f")
	var body []byte
	var err error
	switch f {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		body, err = json.Marshal(&jsonWrapper{Subsonic: payload})
	case "jsonp":
		callback := r.URL.Query().Get("callback")
		if !validJSIdentifier.MatchString(callback) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"subsonic-response":{"status":"failed","version":"` + Version +
				`","type":"` + ServerType + `","serverVersion":"` + rt.ServerVersion +
				`","error":{"code":0,"message":"invalid callback parameter"}}}`))
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		if body, err = json.Marshal(&jsonWrapper{Subsonic: payload}); err == nil {
			body = fmt.Appendf(nil, "%s(%s)", callback, body)
		}
	default:
		w.Header().Set("Content-Type", "application/xml")
		body, err = xml.Marshal(payload)
	}
	if err != nil { // невозможно для наших DTO; на всякий случай — конверт code 0
		log.Printf("subsonic: render %s: %v", f, err)
		rt.sendError(w, r, NewError(ErrorGeneric, "render failed"))
		return
	}
	if status != 0 {
		w.WriteHeader(status)
	}
	_, _ = w.Write(body)
}
