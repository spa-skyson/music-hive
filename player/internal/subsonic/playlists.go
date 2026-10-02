// Плейлисты Subsonic (F3.4, GitLab #26): getPlaylists/getPlaylist/
// createPlaylist/updatePlaylist/deletePlaylist. Плейлисты user-scoped
// (playlists.owner_user_id, F2.2): getPlaylists — только свои (совместное
// использование публичных — вне задачи, см. #8); comment — в
// playlists.meta_json ({"comment": ...}); updated_at у playlists нет,
// changed = created. DTO производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/playlists.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"net/http"
	"strconv"
	"time"
)

// ---------------------------------------------------------------- DTO

// Playlists — ответ getPlaylists.
type Playlists struct {
	Playlist Array[Playlist] `xml:"playlist" json:"playlist"`
}

// Playlist — плейлист; Entries заполняет только getPlaylist (в списке
// omitempty — экономим размер, клиенты за треками идут в getPlaylist).
type Playlist struct {
	ID        string       `xml:"id,attr" json:"id"`
	Name      string       `xml:"name,attr" json:"name"`
	Comment   string       `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Owner     string       `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	Public    bool         `xml:"public,attr" json:"public"`
	SongCount int          `xml:"songCount,attr" json:"songCount"`
	Duration  int          `xml:"duration,attr" json:"duration"`
	Created   string       `xml:"created,attr" json:"created"`
	Changed   string       `xml:"changed,attr" json:"changed"`
	Entries   Array[Child] `xml:"entry,omitempty" json:"entry,omitempty"`
}

// ---------------------------------------------------------------- окно Store

// PlaylistRow — плейлист с агрегатами; Tracks заполняет только
// SubsonicPlaylist (в списке достаточно счётчиков).
type PlaylistRow struct {
	ID        int64
	Name      string
	Comment   string
	OwnerID   int64
	Owner     string
	Public    bool
	SongCount int
	Duration  int
	CreatedAt time.Time
	Tracks    []SongRow
}

// PlaylistStore — user-scoped CRUD плейлистов (реализация — *db.PGStore;
// таблицы playlists/playlist_tracks из схемы 00001, миграций не нужно).
// Мутаторы с isAdmin пропускают админа к чужим плейлистам (deletePlaylist
// по спецификации — владелец или админ).
type PlaylistStore interface {
	SubsonicPlaylists(userID int64) ([]PlaylistRow, error)
	SubsonicPlaylist(id int64) (PlaylistRow, bool, error)
	SubsonicPlaylistCreate(userID int64, name string, songIDs []int64) (PlaylistRow, error)
	// Мутаторы: false = не найден или чужой (хендлер отдаёт 70).
	SubsonicPlaylistReplace(userID, id int64, songIDs []int64, isAdmin bool) (bool, error)
	SubsonicPlaylistUpdate(userID, id int64, name, comment *string, addIDs []int64, removePositions []int, isAdmin bool) (bool, error)
	SubsonicPlaylistDelete(userID, id int64, isAdmin bool) (bool, error)
}

// playlistDTO — PlaylistRow → Playlist.
func playlistDTO(p PlaylistRow) Playlist {
	return Playlist{
		ID:        strconv.FormatInt(p.ID, 10),
		Name:      p.Name,
		Comment:   p.Comment,
		Owner:     p.Owner,
		Public:    p.Public,
		SongCount: p.SongCount,
		Duration:  p.Duration,
		Created:   isoTime(p.CreatedAt),
		Changed:   isoTime(p.CreatedAt), // колонки updated_at нет (схема 00001)
	}
}

// parseSongIDs — мультипараметр songId*: пусто → nil, мусор → 70.
func parseSongIDs(raw []string) ([]int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(raw))
	for _, r := range raw {
		id, err := strconv.ParseInt(r, 10, 64)
		if err != nil || id <= 0 {
			return nil, NewError(ErrorDataNotFound, "")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// checkSongs — существование каждого трека каталога (иначе FK-ошибка
// ушла бы как generic 0); → 70.
func (rt *Router) checkSongs(ids []int64) error {
	for _, id := range ids {
		if err := rt.checkEntity("track", id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- handlers

// getPlaylists — плейлисты текущего пользователя (свои; публичные чужие —
// вопрос владельца, #8).
func (rt *Router) getPlaylists(r *http.Request) (*Subsonic, error) {
	if rt.Playlists == nil {
		return nil, notImplemented("getPlaylists")
	}
	rows, err := rt.Playlists.SubsonicPlaylists(requestUser(r).ID)
	if err != nil {
		return nil, err
	}
	out := make(Array[Playlist], 0, len(rows))
	for _, p := range rows {
		out = append(out, playlistDTO(p))
	}
	resp := rt.NewResponse()
	resp.Playlists = &Playlists{Playlist: out}
	return resp, nil
}

// getPlaylist — плейлист с треками; чужой — только админу (70 у прочих).
func (rt *Router) getPlaylist(r *http.Request) (*Subsonic, error) {
	if rt.Playlists == nil || rt.Catalog == nil {
		return nil, notImplemented("getPlaylist")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	row, found, err := rt.Playlists.SubsonicPlaylist(id)
	if err != nil {
		return nil, err
	}
	u := requestUser(r)
	if !found || (row.OwnerID != u.ID && !u.IsAdmin) {
		return nil, NewError(ErrorDataNotFound, "")
	}
	dto := playlistDTO(row)
	for _, s := range row.Tracks {
		dto.Entries = append(dto.Entries, songChild(s, strconv.FormatInt(s.AlbumID, 10)))
	}
	resp := rt.NewResponse()
	resp.Playlist = &dto
	return resp, nil
}

// createPlaylist — name + songId[] (создание) либо playlistId + songId[]
// (полная замена состава, форма update по спецификации).
func (rt *Router) createPlaylist(r *http.Request) (*Subsonic, error) {
	if rt.Playlists == nil || rt.Catalog == nil {
		return nil, notImplemented("createPlaylist")
	}
	q := r.URL.Query()
	songIDs, err := parseSongIDs(q["songId"])
	if err != nil {
		return nil, err
	}
	if err := rt.checkSongs(songIDs); err != nil {
		return nil, err
	}
	u := requestUser(r)
	if raw := q.Get("playlistId"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return nil, NewError(ErrorDataNotFound, "")
		}
		if ok, err := rt.Playlists.SubsonicPlaylistReplace(u.ID, id, songIDs, u.IsAdmin); err != nil {
			return nil, err
		} else if !ok {
			return nil, NewError(ErrorDataNotFound, "")
		}
		return rt.NewResponse(), nil
	}
	name := q.Get("name")
	if name == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter name is missing")
	}
	row, err := rt.Playlists.SubsonicPlaylistCreate(u.ID, name, songIDs)
	if err != nil {
		return nil, err
	}
	dto := playlistDTO(row)
	resp := rt.NewResponse()
	resp.Playlist = &dto
	return resp, nil
}

// updatePlaylist — playlistId + name/comment (частично) + songIdToAdd[] +
// songIndexToRemove[] (позиции в текущем составе, считая с 0).
func (rt *Router) updatePlaylist(r *http.Request) (*Subsonic, error) {
	if rt.Playlists == nil || rt.Catalog == nil {
		return nil, notImplemented("updatePlaylist")
	}
	q := r.URL.Query()
	id, err := parseEntityID(r, "playlistId")
	if err != nil {
		return nil, err
	}
	var name, comment *string
	if q.Has("name") {
		v := q.Get("name")
		name = &v
	}
	if q.Has("comment") {
		v := q.Get("comment")
		comment = &v
	}
	addIDs, err := parseSongIDs(q["songIdToAdd"])
	if err != nil {
		return nil, err
	}
	if err := rt.checkSongs(addIDs); err != nil {
		return nil, err
	}
	var remove []int
	for _, raw := range q["songIndexToRemove"] {
		pos, err := strconv.Atoi(raw)
		if err != nil || pos < 0 {
			return nil, NewError(ErrorMissingParameter, "invalid songIndexToRemove")
		}
		remove = append(remove, pos)
	}
	u := requestUser(r)
	if ok, err := rt.Playlists.SubsonicPlaylistUpdate(u.ID, id, name, comment, addIDs, remove, u.IsAdmin); err != nil {
		return nil, err
	} else if !ok {
		return nil, NewError(ErrorDataNotFound, "")
	}
	return rt.NewResponse(), nil
}

// deletePlaylist — владелец или админ.
func (rt *Router) deletePlaylist(r *http.Request) (*Subsonic, error) {
	if rt.Playlists == nil {
		return nil, notImplemented("deletePlaylist")
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	u := requestUser(r)
	if ok, err := rt.Playlists.SubsonicPlaylistDelete(u.ID, id, u.IsAdmin); err != nil {
		return nil, err
	} else if !ok {
		return nil, NewError(ErrorDataNotFound, "")
	}
	return rt.NewResponse(), nil
}
