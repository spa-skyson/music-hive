// Поиск Subsonic (F3.3, GitLab #25): search2/search3 — ILIKE-подстрока по
// имени артиста, названию альбома/трека с независимыми count/offset на блок.
// Legacy search (v1.0-параметры artist/album/title) остаётся 501: живые
// клиенты не используют. DTO производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/searching.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"net/http"
	"strconv"
)

// SearchResult — ответ search2/search3 (имя корневого элемента задаёт
// поле конверта).
type SearchResult struct {
	Artist Array[ArtistID3] `xml:"artist" json:"artist"`
	Album  Array[AlbumID3]  `xml:"album" json:"album"`
	Song   Array[Child]     `xml:"song" json:"song"`
}

// search — /rest/search (самая старая форма параметров): 501.
func (rt *Router) search(_ *http.Request) (*Subsonic, error) {
	return nil, notImplemented("search")
}

// search2 — /rest/search2.
func (rt *Router) search2(r *http.Request) (*Subsonic, error) {
	out, err := rt.runSearch(r)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	resp.Search2 = out
	return resp, nil
}

// search3 — /rest/search3 (ID3-элементы; блоки те же).
func (rt *Router) search3(r *http.Request) (*Subsonic, error) {
	out, err := rt.runSearch(r)
	if err != nil {
		return nil, err
	}
	resp := rt.NewResponse()
	resp.Search3 = out
	return resp, nil
}

// runSearch — общая реализация: query обязателен, счётчики блоков
// artistCount/albumCount/songCount (default 20, max 500) и смещения
// artistOffset/albumOffset/songOffset.
func (rt *Router) runSearch(r *http.Request) (*SearchResult, error) {
	if rt.Catalog == nil {
		return nil, notImplemented("search2")
	}
	q := r.URL.Query()
	query := q.Get("query")
	if query == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter query is missing")
	}
	artists, err := rt.Catalog.SubsonicSearchArtists(query,
		clampSize(q.Get("artistCount")), clampOffset(q.Get("artistOffset")))
	if err != nil {
		return nil, err
	}
	albums, err := rt.Catalog.SubsonicSearchAlbums(query,
		clampSize(q.Get("albumCount")), clampOffset(q.Get("albumOffset")))
	if err != nil {
		return nil, err
	}
	songs, err := rt.Catalog.SubsonicSearchSongs(query,
		clampSize(q.Get("songCount")), clampOffset(q.Get("songOffset")))
	if err != nil {
		return nil, err
	}
	out := &SearchResult{}
	for _, a := range artists {
		out.Artist = append(out.Artist, newArtistID3(a))
	}
	for _, a := range albums {
		out.Album = append(out.Album, newAlbumID3(a))
	}
	for _, s := range songs {
		out.Song = append(out.Song, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	return out, nil
}
