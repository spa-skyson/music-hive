// Прочие эндпоинты Subsonic (F3.4, GitLab #26): закладки ↔
// user_listen_later («послушать позже»), интернет-радио ↔ radio_shares
// (streamUrl = PublicBaseURL + /listen/<token>.mp3), getUser/getUsers,
// getScanStatus/startScan, getSimilarSongs(2), getAvatar, getTopSongs.
// DTO производны от Navidrome (https://github.com/navidrome/navidrome,
// server/subsonic/{bookmarks,internet_radio,users,scan_status}.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// ---------------------------------------------------------------- DTO

// Bookmarks — ответ getBookmarks.
type Bookmarks struct {
	Bookmark Array[Bookmark] `xml:"bookmark" json:"bookmark"`
}

// Bookmark — закладка = трек из user_listen_later. Position (мс внутри
// трека) и Comment не хранятся — колонок нет, отдаём 0/пусто (матрица).
type Bookmark struct {
	Position int64  `xml:"position,attr" json:"position"`
	Username string `xml:"username,attr" json:"username"`
	Comment  string `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Created  string `xml:"created,attr" json:"created"`
	Changed  string `xml:"changed,attr" json:"changed"`
	Child
}

// InternetRadioStations — ответ getInternetRadioStations.
type InternetRadioStations struct {
	Station Array[InternetRadioStation] `xml:"internetRadioStation" json:"internetRadioStation"`
}

// InternetRadioStation — share-радио как внешняя станция: streamUrl —
// публичный /listen/<token>.mp3, homepage — корень сервера. Произвольные
// внешние streamUrl не хранятся (в radio_shares нет колонки) — станция
// всегда ведёт на наш стрим.
type InternetRadioStation struct {
	ID          string `xml:"id,attr" json:"id"`
	Name        string `xml:"name,attr" json:"name"`
	StreamURL   string `xml:"streamUrl,attr" json:"streamUrl"`
	HomepageURL string `xml:"homepageUrl,attr" json:"homepageUrl"`
}

// User — блок ролей getUser/getUsers. Роли по матрице F3.4:
// adminRole/shareRole = is_admin, settings/stream/download/playlist/
// comment = true, jukebox/upload/coverArt/podcast/videoConversion = false.
type User struct {
	Username            string `xml:"username,attr" json:"username"`
	ScrobblingEnabled   bool   `xml:"scrobblingEnabled,attr" json:"scrobblingEnabled"`
	AdminRole           bool   `xml:"adminRole,attr" json:"adminRole"`
	SettingsRole        bool   `xml:"settingsRole,attr" json:"settingsRole"`
	DownloadRole        bool   `xml:"downloadRole,attr" json:"downloadRole"`
	UploadRole          bool   `xml:"uploadRole,attr" json:"uploadRole"`
	PlaylistRole        bool   `xml:"playlistRole,attr" json:"playlistRole"`
	CoverArtRole        bool   `xml:"coverArtRole,attr" json:"coverArtRole"`
	CommentRole         bool   `xml:"commentRole,attr" json:"commentRole"`
	PodcastRole         bool   `xml:"podcastRole,attr" json:"podcastRole"`
	StreamRole          bool   `xml:"streamRole,attr" json:"streamRole"`
	JukeboxRole         bool   `xml:"jukeboxRole,attr" json:"jukeboxRole"`
	ShareRole           bool   `xml:"shareRole,attr" json:"shareRole"`
	VideoConversionRole bool   `xml:"videoConversionRole,attr" json:"videoConversionRole"`
	Folder              []int  `xml:"folder" json:"folder"`
}

// Users — ответ getUsers.
type Users struct {
	User Array[User] `xml:"user" json:"user"`
}

// ScanStatus — ответ getScanStatus.
type ScanStatus struct {
	Scanning bool  `xml:"scanning,attr" json:"scanning"`
	Count    int64 `xml:"count,attr" json:"count"`
}

// SimilarSongs / SimilarSongs2 — ответы getSimilarSongs(2).
type SimilarSongs struct {
	Song Array[Child] `xml:"song" json:"song"`
}

// SimilarSongs2 — то же самое тело (элемент конверта другой).
type SimilarSongs2 = SimilarSongs

// ---------------------------------------------------------------- окна Store

// BookmarkRow — строка user_listen_later.
type BookmarkRow struct {
	TrackID int64
	AddedAt time.Time
}

// BookmarkStore — закладки ↔ user_listen_later (реализация — *db.PGStore
// поверх LaterList/LaterAdd/LaterRemove).
type BookmarkStore interface {
	SubsonicBookmarks(userID int64) ([]BookmarkRow, error)
	SubsonicBookmarkAdd(userID, trackID int64) error
	SubsonicBookmarkRemove(userID, trackID int64) error
}

// RadioStationRow — активный radio_share.
type RadioStationRow struct {
	Token   string
	Name    string
	OwnerID int64
}

// RadioStore — интернет-радио ↔ radio_shares: create — новый share-токен
// (токен генерирует хендлер), rename/delete — владелец или админ.
type RadioStore interface {
	SubsonicRadioList(userID int64) ([]RadioStationRow, error)
	SubsonicRadioCreate(userID int64, token, name string) (RadioStationRow, error)
	SubsonicRadioGet(token string) (RadioStationRow, bool, error)
	SubsonicRadioRename(token, name string) error
	SubsonicRadioRevoke(token string) error
}

// UserAccounts — список пользователей (реализация — *db.PGStore:
// AdminListUsers из admin-API F2.3, без новых SQL).
type UserAccounts interface {
	AdminListUsers() ([]auth.AdminUser, error)
}

// ScanStore — статус сканирования и постановка job в очередь
// (SubsonicScanStatus — новый SQL; EnqueueJob — существующий Backend).
type ScanStore interface {
	SubsonicScanStatus() (count int64, scanning bool, err error)
	EnqueueJob(kind, payloadJSON string) (int64, error)
}

// SimilarFunc — мост к recommend-слою: kind ∈ {track, artist, album},
// id — сущность сида, count — лимит; возвращает id похожих треков.
// Отдельная функция, а не импорт recommend: он тянет index, а index —
// db, и цикл db→subsonic→index→db замкнулся бы.
type SimilarFunc func(kind string, id int64, count int) []int64

// ---------------------------------------------------------------- helpers

// randomHexToken — токен share-радио (128 бит, как /api/share/radio).
func randomHexToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// publicBase — база публичных URL станций: конфиг PublicBaseURL либо
// схема+host запроса (как api.Server.publicBase).
func (rt *Router) publicBase(r *http.Request) string {
	if rt.PublicBaseURL != "" {
		return rt.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8787"
	}
	return scheme + "://" + host
}

func userDTO(a auth.AdminUser) User {
	admin := a.IsAdmin || a.IsOwner
	return User{
		Username:            a.Username,
		ScrobblingEnabled:   true,
		AdminRole:           admin,
		SettingsRole:        true,
		DownloadRole:        true,
		UploadRole:          false,
		PlaylistRole:        true,
		CoverArtRole:        false,
		CommentRole:         true,
		PodcastRole:         false,
		StreamRole:          true,
		JukeboxRole:         false,
		ShareRole:           admin,
		VideoConversionRole: false,
		Folder:              []int{1},
	}
}

// ---------------------------------------------------------------- bookmarks

// getBookmarks — «послушать позже» текущего пользователя.
func (rt *Router) getBookmarks(r *http.Request) (*Subsonic, error) {
	if rt.Bookmarks == nil || rt.Catalog == nil {
		return nil, notImplemented("getBookmarks")
	}
	rows, err := rt.Bookmarks.SubsonicBookmarks(requestUser(r).ID)
	if err != nil {
		return nil, err
	}
	username := requestUser(r).Username
	out := make(Array[Bookmark], 0, len(rows))
	for _, row := range rows {
		song, ok, err := rt.Catalog.SubsonicSong(row.TrackID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		child := songChild(song, strconv.FormatInt(song.AlbumID, 10))
		out = append(out, Bookmark{
			Username: username,
			Created:  isoTime(row.AddedAt),
			Changed:  isoTime(row.AddedAt),
			Child:    child,
		})
	}
	resp := rt.NewResponse()
	resp.Bookmarks = &Bookmarks{Bookmark: out}
	return resp, nil
}

// createBookmark — id + position + comment (position/comment не храним:
// у user_listen_later position — порядок, а не мс; колонки comment нет).
func (rt *Router) createBookmark(r *http.Request) (*Subsonic, error) {
	if rt.Bookmarks == nil || rt.Catalog == nil {
		return nil, notImplemented("createBookmark")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	if err := rt.checkEntity("track", id); err != nil {
		return nil, err
	}
	if err := rt.Bookmarks.SubsonicBookmarkAdd(requestUser(r).ID, id); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// deleteBookmark — id (идемпотентно: нет закладки → ок).
func (rt *Router) deleteBookmark(r *http.Request) (*Subsonic, error) {
	if rt.Bookmarks == nil {
		return nil, notImplemented("deleteBookmark")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	if err := rt.Bookmarks.SubsonicBookmarkRemove(requestUser(r).ID, id); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// ---------------------------------------------------------------- radio

// getInternetRadioStations — свои активные share-радио.
func (rt *Router) getInternetRadioStations(r *http.Request) (*Subsonic, error) {
	if rt.Radio == nil {
		return nil, notImplemented("getInternetRadioStations")
	}
	shares, err := rt.Radio.SubsonicRadioList(requestUser(r).ID)
	if err != nil {
		return nil, err
	}
	base := rt.publicBase(r)
	out := make(Array[InternetRadioStation], 0, len(shares))
	for _, sh := range shares {
		out = append(out, InternetRadioStation{
			ID:          sh.Token,
			Name:        sh.Name,
			StreamURL:   base + "/listen/" + sh.Token + ".mp3",
			HomepageURL: base,
		})
	}
	resp := rt.NewResponse()
	resp.InternetRadioStations = &InternetRadioStations{Station: out}
	return resp, nil
}

// createInternetRadioStation — создаёт share-токен; name обязателен,
// streamUrl/homepageUrl клиента игнорируются (станция = наш стрим).
func (rt *Router) createInternetRadioStation(r *http.Request) (*Subsonic, error) {
	if rt.Radio == nil {
		return nil, notImplemented("createInternetRadioStation")
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter name is missing")
	}
	token, err := randomHexToken()
	if err != nil {
		return nil, err
	}
	if _, err := rt.Radio.SubsonicRadioCreate(requestUser(r).ID, token, name); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// updateInternetRadioStation — переименование (id → token share);
// владелец или админ, иначе 50; несуществующая → 70.
func (rt *Router) updateInternetRadioStation(r *http.Request) (*Subsonic, error) {
	if rt.Radio == nil {
		return nil, notImplemented("updateInternetRadioStation")
	}
	token := r.URL.Query().Get("id")
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if token == "" || name == "" {
		return nil, NewError(ErrorMissingParameter, "required parameters id and name are missing")
	}
	if err := rt.radioMutationAllowed(r, token); err != nil {
		return nil, err
	}
	if err := rt.Radio.SubsonicRadioRename(token, name); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// deleteInternetRadioStation — отзыв share (не физическое удаление:
// история listen_count остаётся); владелец или админ.
func (rt *Router) deleteInternetRadioStation(r *http.Request) (*Subsonic, error) {
	if rt.Radio == nil {
		return nil, notImplemented("deleteInternetRadioStation")
	}
	token := r.URL.Query().Get("id")
	if token == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter id is missing")
	}
	if err := rt.radioMutationAllowed(r, token); err != nil {
		return nil, err
	}
	if err := rt.Radio.SubsonicRadioRevoke(token); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// radioMutationAllowed — станция существует (70) и она своя/админская (50).
func (rt *Router) radioMutationAllowed(r *http.Request, token string) error {
	sh, found, err := rt.Radio.SubsonicRadioGet(token)
	if err != nil {
		return err
	}
	if !found {
		return NewError(ErrorDataNotFound, "")
	}
	if u := requestUser(r); sh.OwnerID != u.ID && !u.IsAdmin {
		return NewError(ErrorAuthorizationFail, "")
	}
	return nil
}

// ---------------------------------------------------------------- users

// getUser — свой блок (username не задан) или любой для админа (иначе 50).
func (rt *Router) getUser(r *http.Request) (*Subsonic, error) {
	if rt.Accounts == nil {
		return nil, notImplemented("getUser")
	}
	u := requestUser(r)
	name := r.URL.Query().Get("username")
	if name == "" {
		name = u.Username
	}
	if name != u.Username && !u.IsAdmin && !u.IsOwner {
		return nil, NewError(ErrorAuthorizationFail, "")
	}
	accounts, err := rt.Accounts.AdminListUsers()
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if strings.EqualFold(a.Username, name) {
			dto := userDTO(a)
			resp := rt.NewResponse()
			resp.User = &dto
			return resp, nil
		}
	}
	return nil, NewError(ErrorDataNotFound, "")
}

// getUsers — только админ (50 прочим).
func (rt *Router) getUsers(r *http.Request) (*Subsonic, error) {
	if rt.Accounts == nil {
		return nil, notImplemented("getUsers")
	}
	if u := requestUser(r); !u.IsAdmin && !u.IsOwner {
		return nil, NewError(ErrorAuthorizationFail, "")
	}
	accounts, err := rt.Accounts.AdminListUsers()
	if err != nil {
		return nil, err
	}
	out := make(Array[User], 0, len(accounts))
	for _, a := range accounts {
		out = append(out, userDTO(a))
	}
	resp := rt.NewResponse()
	resp.Users = &Users{User: out}
	return resp, nil
}

// ---------------------------------------------------------------- scan

// getScanStatus — count активных треков + scanning по jobs scan/full_rescan.
func (rt *Router) getScanStatus(_ *http.Request) (*Subsonic, error) {
	if rt.Scan == nil {
		return nil, notImplemented("getScanStatus")
	}
	count, scanning, err := rt.Scan.SubsonicScanStatus()
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	resp.ScanStatus = &ScanStatus{Scanning: scanning, Count: count}
	return resp, nil
}

// startScan — админ; ставит full_rescan в очередь (воркер подхватит).
func (rt *Router) startScan(r *http.Request) (*Subsonic, error) {
	if rt.Scan == nil {
		return nil, notImplemented("startScan")
	}
	if u := requestUser(r); !u.IsAdmin && !u.IsOwner {
		return nil, NewError(ErrorAuthorizationFail, "")
	}
	if _, err := rt.Scan.EnqueueJob("full_rescan", ""); err != nil {
		return nil, err
	}
	return rt.getScanStatus(r)
}

// ---------------------------------------------------------------- similar

// getSimilarSongs / getSimilarSongs2 — id разбирается как трек → артист →
// альбом (как setRating); похожесть считает SimilarFunc (recommend),
// треки добираются из каталога.
func (rt *Router) getSimilarSongs(r *http.Request) (*Subsonic, error) {
	songs, err := rt.similarSongs(r)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	resp.SimilarSongs = songs
	return resp, nil
}

func (rt *Router) getSimilarSongs2(r *http.Request) (*Subsonic, error) {
	songs, err := rt.similarSongs(r)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	resp.SimilarSongs2 = songs
	return resp, nil
}

func (rt *Router) similarSongs(r *http.Request) (*SimilarSongs, error) {
	if rt.Similar == nil || rt.Catalog == nil {
		return nil, notImplemented("getSimilarSongs")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	count := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && v > 0 {
		count = v
	}
	kind := "track"
	if _, found, err := rt.Catalog.SubsonicSong(id); err != nil {
		return nil, err
	} else if !found {
		if _, found, err = rt.Catalog.SubsonicArtist(id); err != nil {
			return nil, err
		} else if found {
			kind = "artist"
		} else if _, found, err = rt.Catalog.SubsonicAlbum(id); err != nil {
			return nil, err
		} else if found {
			kind = "album"
		} else {
			return nil, NewError(ErrorDataNotFound, "")
		}
	}
	out := &SimilarSongs{Song: Array[Child]{}}
	for _, hitID := range rt.Similar(kind, id, count) {
		song, found, err := rt.Catalog.SubsonicSong(hitID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		out.Song = append(out.Song, songChild(song, strconv.FormatInt(song.AlbumID, 10)))
	}
	return out, nil
}

// ---------------------------------------------------------------- заглушки

// getTopSongs — данных популярности исполнителя нет → 501 (матрица).
func (rt *Router) getTopSongs(_ *http.Request) (*Subsonic, error) {
	return nil, notImplemented("getTopSongs")
}

// getAvatar — аватаров нет → 70 (матрица).
func (rt *Router) getAvatar(_ *http.Request) (*Subsonic, error) {
	return nil, NewError(ErrorDataNotFound, "user avatars are not supported")
}
