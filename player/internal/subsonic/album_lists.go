// Списки альбомов и подборки песен Subsonic: getAlbumList (legacy) /
// getAlbumList2 (ID3), getRandomSongs, getSongsByGenre. DTO и разбор
// параметров производны от Navidrome (https://github.com/navidrome/navidrome,
// server/subsonic/album_lists.go), © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------- DTO

// AlbumList — legacy getAlbumList: альбомы как child-псевдо-директории.
type AlbumList struct {
	Album Array[Child] `xml:"album" json:"album"`
}

// AlbumList2 — ID3 getAlbumList2.
type AlbumList2 struct {
	Album Array[AlbumID3] `xml:"album" json:"album"`
}

// RandomSongs — ответ getRandomSongs.
type RandomSongs struct {
	Song Array[Child] `xml:"song" json:"song"`
}

// SongsByGenre — ответ getSongsByGenre.
type SongsByGenre struct {
	Song Array[Child] `xml:"song" json:"song"`
}

// ---------------------------------------------------------------- params

// libListTypes — типы с особыми источниками F3.3 (#25): starred →
// user_favorites, frequent → user_track_stats.completed, highest →
// user_ratings (avg), recent → альбомы по max(track.created_at).
var libListTypes = map[string]bool{
	"recent": true, "frequent": true, "highest": true, "starred": true,
}

// sqlListTypes — типы, транслируемые в SQL (db.SubsonicAlbumList).
var sqlListTypes = map[string]bool{
	"random": true, "newest": true, "alphabeticalByName": true,
	"alphabeticalArtist": true, "byYear": true, "byGenre": true,
}

// clampSize — size/count-параметры: default 10, максимум 500 (спецификация).
func clampSize(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 10
	}
	if n > 500 {
		return 500
	}
	return n
}

// clampOffset — offset-параметр: >= 0, мусор = 0.
func clampOffset(raw string) int {
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return n
	}
	return 0
}

// parseAlbumListParams — общие параметры getAlbumList/getAlbumList2.
type albumListParams struct {
	query AlbumListQuery
}

func parseAlbumListParams(r *http.Request) (albumListParams, error) {
	q := r.URL.Query()
	p := albumListParams{query: AlbumListQuery{
		Type:   q.Get("type"),
		Genre:  q.Get("genre"),
		Size:   clampSize(q.Get("size")),
		Offset: clampOffset(q.Get("offset")),
	}}
	switch {
	case p.query.Type == "":
		return p, NewError(ErrorMissingParameter, "required parameter type is missing")
	case libListTypes[p.query.Type], sqlListTypes[p.query.Type]:
		// ок, источник выбирает albumRows
	default:
		return p, NewError(ErrorMissingParameter, "unsupported type "+p.query.Type)
	}
	if p.query.Type == "byGenre" && p.query.Genre == "" {
		return p, NewError(ErrorMissingParameter, "required parameter genre is missing")
	}
	if p.query.Type == "byYear" {
		for _, name := range []string{"fromYear", "toYear"} {
			v, err := strconv.Atoi(q.Get(name))
			if q.Get(name) == "" || err != nil {
				return p, NewError(ErrorMissingParameter, "required parameter "+name+" is missing")
			}
			if name == "fromYear" {
				p.query.FromYear = v
			} else {
				p.query.ToYear = v
			}
		}
	}
	return p, nil
}

// albumRows — источник строк по типу списка: SQL-типы → Catalog.SubsonicAlbumList,
// специальные (F3.3) → starred/frequent/highest/recent. Starred-списки без
// пагинации в SQL (getStarred возвращает всё) — режем здесь.
func (rt *Router) albumRows(r *http.Request, p albumListParams) ([]AlbumRow, error) {
	if libListTypes[p.query.Type] && (rt.Library == nil || rt.Catalog == nil) {
		return nil, notImplemented("getAlbumList type " + p.query.Type)
	}
	switch p.query.Type {
	case "starred":
		rows, err := rt.Library.SubsonicStarredAlbums(requestUser(r).ID)
		return pageRows(rows, p.query.Offset, p.query.Size), err
	case "frequent":
		return rt.Library.SubsonicFrequentAlbums(requestUser(r).ID, p.query.Size, p.query.Offset)
	case "highest":
		return rt.Catalog.SubsonicHighestAlbums(p.query.Size, p.query.Offset)
	case "recent":
		return rt.Catalog.SubsonicRecentAlbums(p.query.Size, p.query.Offset)
	}
	return rt.Catalog.SubsonicAlbumList(p.query)
}

// pageRows — срез [offset, offset+size) для непагинируемых источников.
func pageRows[T any](rows []T, offset, size int) []T {
	if offset >= len(rows) {
		return nil
	}
	rows = rows[offset:]
	if len(rows) > size {
		rows = rows[:size]
	}
	return rows
}

// ---------------------------------------------------------------- handlers

// getAlbumList — legacy: альбомы-псевдо-директории al-<id> (навигация
// клиента дальше — getMusicDirectory).
func (rt *Router) getAlbumList(r *http.Request) (*Subsonic, error) {
	p, err := parseAlbumListParams(r)
	if err != nil {
		return nil, err
	}
	albums, err := rt.albumRows(r, p)
	if err != nil {
		return nil, err
	}
	list := &AlbumList{Album: Array[Child]{}}
	for _, a := range albums {
		list.Album = append(list.Album, albumDirChild(a))
	}
	resp := rt.NewResponse()
	resp.AlbumList = list
	return resp, nil
}

// getAlbumList2 — ID3-альбомы.
func (rt *Router) getAlbumList2(r *http.Request) (*Subsonic, error) {
	p, err := parseAlbumListParams(r)
	if err != nil {
		return nil, err
	}
	albums, err := rt.albumRows(r, p)
	if err != nil {
		return nil, err
	}
	list := &AlbumList2{Album: Array[AlbumID3]{}}
	for _, a := range albums {
		list.Album = append(list.Album, newAlbumID3(a))
	}
	resp := rt.NewResponse()
	resp.AlbumList2 = list
	return resp, nil
}

// getRandomSongs — случайные треки (фильтры genre/fromYear/toYear
// спецификации не поддерживаются — ROADMAP с #25).
func (rt *Router) getRandomSongs(r *http.Request) (*Subsonic, error) {
	size := clampSize(r.URL.Query().Get("size"))
	songs, err := rt.Catalog.SubsonicRandomSongs(size)
	if err != nil {
		return nil, err
	}
	list := &RandomSongs{Song: Array[Child]{}}
	for _, s := range songs {
		list.Song = append(list.Song, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	resp := rt.NewResponse()
	resp.RandomSongs = list
	return resp, nil
}

// getSongsByGenre — треки жанра (genre — значение из getGenres).
func (rt *Router) getSongsByGenre(r *http.Request) (*Subsonic, error) {
	q := r.URL.Query()
	genre := q.Get("genre")
	if genre == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter genre is missing")
	}
	songs, err := rt.Catalog.SubsonicSongsByGenre(genre, clampSize(q.Get("count")), clampOffset(q.Get("offset")))
	if err != nil {
		return nil, err
	}
	list := &SongsByGenre{Song: Array[Child]{}}
	for _, s := range songs {
		list.Song = append(list.Song, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	resp := rt.NewResponse()
	resp.SongsByGenre = list
	return resp, nil
}
