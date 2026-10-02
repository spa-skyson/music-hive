// Медиа-аннотации Subsonic (F3.3, GitLab #25): scrobble, star/unstar,
// setRating, getStarred/getStarred2. Пользователь — из subsonic-auth
// (user-scoped таблицы user_favorites/user_ratings/listening_history).
// DTO производны от Navidrome (https://github.com/navidrome/navidrome),
// server/subsonic/annotation.go, © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// ---------------------------------------------------------------- DTO

// Starred — legacy getStarred: все блоки как Child.
type Starred struct {
	Artist Array[Child] `xml:"artist" json:"artist"`
	Album  Array[Child] `xml:"album" json:"album"`
	Song   Array[Child] `xml:"song" json:"song"`
}

// Starred2 — ID3 getStarred2.
type Starred2 struct {
	Artist Array[ArtistID3] `xml:"artist" json:"artist"`
	Album  Array[AlbumID3]  `xml:"album" json:"album"`
	Song   Array[Child]     `xml:"song" json:"song"`
}

// ---------------------------------------------------------------- helpers

// requestUser — пользователь subsonic-auth (мидлварь гарантирует наличие).
func requestUser(r *http.Request) auth.User {
	u, _ := auth.FromContext(r.Context())
	return u
}

// ---------------------------------------------------------------- handlers

// scrobble — фиксация прослушивания: id (или повторенные ids) →
// listening_history (action=track_end, source=subsonic — как /api/events
// track_end) + completed в user_track_stats (частота для frequent).
// time[] — мс эпохи, поэлементно к ids; submission=false (now-playing)
// ничего не пишет — ok без side-эффектов.
func (rt *Router) scrobble(r *http.Request) (*Subsonic, error) {
	if rt.Library == nil || rt.Catalog == nil {
		return nil, notImplemented("scrobble")
	}
	q := r.URL.Query()
	ids := append(q["id"], q["ids"]...)
	if len(ids) == 0 {
		return nil, NewError(ErrorMissingParameter, "required parameter id is missing")
	}
	if q.Get("submission") == "false" {
		return rt.NewResponse(), nil
	}
	times := q["time"]
	uid := requestUser(r).ID
	for i, raw := range ids {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return nil, NewError(ErrorDataNotFound, "")
		}
		if _, found, err := rt.Catalog.SubsonicSong(id); err != nil {
			return nil, err
		} else if !found {
			return nil, NewError(ErrorDataNotFound, "")
		}
		at := time.Now()
		if i < len(times) {
			if ms, err := strconv.ParseInt(times[i], 10, 64); err == nil && ms > 0 {
				at = time.UnixMilli(ms).UTC()
			}
		}
		if err := rt.Library.SubsonicScrobble(uid, id, at); err != nil {
			return nil, err
		}
	}
	return rt.NewResponse(), nil
}

// star/unstar — мультипараметры id (трек) / albumId / artistId →
// user_favorites; несуществующая сущность → 70.
func (rt *Router) star(r *http.Request) (*Subsonic, error) {
	return rt.setStarred(r, true)
}

func (rt *Router) unstar(r *http.Request) (*Subsonic, error) {
	return rt.setStarred(r, false)
}

func (rt *Router) setStarred(r *http.Request, starred bool) (*Subsonic, error) {
	if rt.Library == nil || rt.Catalog == nil {
		return nil, notImplemented("star")
	}
	q := r.URL.Query()
	targets := []struct {
		kind string
		ids  []string
	}{
		{"track", q["id"]},
		{"album", q["albumId"]},
		{"artist", q["artistId"]},
	}
	uid := requestUser(r).ID
	any := false
	for _, group := range targets {
		for _, raw := range group.ids {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return nil, NewError(ErrorDataNotFound, "")
			}
			if err := rt.checkEntity(group.kind, id); err != nil {
				return nil, err
			}
			if err := rt.Library.SubsonicFavoriteSet(uid, group.kind, id, starred); err != nil {
				return nil, err
			}
			any = true
		}
	}
	if !any {
		return nil, NewError(ErrorMissingParameter,
			"required parameter id/albumId/artistId is missing")
	}
	return rt.NewResponse(), nil
}

// checkEntity — существование цели star/setRating (иначе FK-ошибка ушла бы
// как generic 0); → 70.
func (rt *Router) checkEntity(kind string, id int64) error {
	var found bool
	var err error
	switch kind {
	case "track":
		_, found, err = rt.Catalog.SubsonicSong(id)
	case "album":
		_, found, err = rt.Catalog.SubsonicAlbum(id)
	case "artist":
		_, found, err = rt.Catalog.SubsonicArtist(id)
	}
	if err != nil {
		return err
	}
	if !found {
		return NewError(ErrorDataNotFound, "")
	}
	return nil
}

// setRating — id (трек/альбом/артист — kind определяется наличием в
// таблицах) + rating 0–5 (0 = удалить). user_ratings из схемы 00001.
func (rt *Router) setRating(r *http.Request) (*Subsonic, error) {
	if rt.Library == nil || rt.Catalog == nil {
		return nil, notImplemented("setRating")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	raw := r.URL.Query().Get("rating")
	if raw == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter rating is missing")
	}
	rating, err := strconv.Atoi(raw)
	if err != nil || rating < 0 || rating > 5 {
		return nil, NewError(ErrorMissingParameter, "rating must be an integer between 0 and 5")
	}
	for _, kind := range []string{"track", "album", "artist"} {
		if err := rt.checkEntity(kind, id); err != nil {
			var se *codedError
			if errors.As(err, &se) && se.code == ErrorDataNotFound {
				continue // не этот kind — пробуем следующий
			}
			return nil, err
		}
		if err := rt.Library.SubsonicRatingSet(requestUser(r).ID, kind, id, rating); err != nil {
			return nil, err
		}
		return rt.NewResponse(), nil
	}
	return nil, NewError(ErrorDataNotFound, "")
}

// getStarred — legacy: звёздное текущего пользователя, всё как Child.
func (rt *Router) getStarred(r *http.Request) (*Subsonic, error) {
	data, err := rt.starredPayload(r)
	if err != nil {
		return nil, err
	}
	out := &Starred{}
	for _, s := range data.songs {
		out.Song = append(out.Song, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	for _, a := range data.albums {
		out.Album = append(out.Album, albumDirChild(a))
	}
	for _, a := range data.artists {
		out.Artist = append(out.Artist, Child{
			ID:       dirID("ar", a.ID),
			IsDir:    true,
			Title:    a.Name,
			CoverArt: dirID("ar", a.ID),
		})
	}
	resp := rt.NewResponse()
	resp.Starred = out
	return resp, nil
}

// getStarred2 — ID3-форма: ArtistID3/AlbumID3/Child.
func (rt *Router) getStarred2(r *http.Request) (*Subsonic, error) {
	data, err := rt.starredPayload(r)
	if err != nil {
		return nil, err
	}
	out := &Starred2{}
	for _, a := range data.artists {
		out.Artist = append(out.Artist, newArtistID3(a))
	}
	for _, a := range data.albums {
		out.Album = append(out.Album, newAlbumID3(a))
	}
	for _, s := range data.songs {
		out.Song = append(out.Song, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	resp := rt.NewResponse()
	resp.Starred2 = out
	return resp, nil
}

type starredData struct {
	artists []ArtistRow
	albums  []AlbumRow
	songs   []SongRow
}

func (rt *Router) starredPayload(r *http.Request) (starredData, error) {
	if rt.Library == nil {
		return starredData{}, notImplemented("getStarred")
	}
	uid := requestUser(r).ID
	var out starredData
	var err error
	if out.artists, err = rt.Library.SubsonicStarredArtists(uid); err != nil {
		return out, err
	}
	if out.albums, err = rt.Library.SubsonicStarredAlbums(uid); err != nil {
		return out, err
	}
	if out.songs, err = rt.Library.SubsonicStarredSongs(uid); err != nil {
		return out, err
	}
	return out, nil
}
