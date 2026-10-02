package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *PGStore) PlaylistMeta(userID int64, kind string) (id int64, name string, n int, err error) {
	err = s.DB.QueryRow(`
SELECT p.id, p.name,
  (SELECT COUNT(*) FROM playlist_tracks pt WHERE pt.playlist_id = p.id)
FROM playlists p
WHERE p.kind = $1 AND p.owner_user_id = $2
ORDER BY p.id DESC LIMIT 1`, kind, userID).Scan(&id, &name, &n)
	if err == sql.ErrNoRows {
		return 0, "", 0, nil
	}
	return
}

func (s *PGStore) LaterList(userID int64) ([]PlaylistTrack, error) {
	return s.playlistTracks(`
SELECT l.position, l.track_id, COALESCE(t.artist,''), COALESCE(t.title,''),
       COALESCE(t.duration,0), ''
FROM user_listen_later l
JOIN tracks t ON t.id = l.track_id
WHERE l.user_id = $1
ORDER BY l.position ASC, l.added_at DESC`, userID)
}

func (s *PGStore) LaterAdd(userID int64, trackID int64) error {
	_, err := s.DB.Exec(`
INSERT INTO user_listen_later(user_id, track_id, added_at, position)
VALUES ($1,$2,now(), COALESCE((SELECT MAX(position) FROM user_listen_later WHERE user_id = $1),0)+1)
ON CONFLICT(user_id, track_id) DO UPDATE SET added_at = now()`, userID, trackID)
	return err
}

func (s *PGStore) LaterRemove(userID int64, trackID int64) error {
	_, err := s.DB.Exec(
		`DELETE FROM user_listen_later WHERE user_id = $1 AND track_id = $2`, userID, trackID)
	return err
}

func (s *PGStore) LaterCount(userID int64) int {
	var n int
	_ = s.DB.QueryRow(
		`SELECT COUNT(*) FROM user_listen_later WHERE user_id = $1`, userID).Scan(&n)
	return n
}

func (s *PGStore) FavoritesList(userID int64) ([]PlaylistTrack, error) {
	return s.playlistTracks(`
SELECT f.position, f.track_id, COALESCE(t.artist,''), COALESCE(t.title,''),
       COALESCE(t.duration,0), ''
FROM user_favorites f
JOIN tracks t ON t.id = f.track_id
WHERE f.user_id = $1 AND f.kind = 'track'
ORDER BY f.position ASC, f.created_at DESC`, userID)
}

func (s *PGStore) FavoritesAdd(userID int64, trackID int64) error {
	_, err := s.DB.Exec(`
INSERT INTO user_favorites(user_id, kind, track_id, position, created_at)
VALUES ($1,'track',$2, COALESCE((SELECT MAX(position) FROM user_favorites
                                  WHERE user_id = $1 AND kind = 'track'),0)+1, now())
ON CONFLICT (user_id, kind, COALESCE(track_id,0), COALESCE(artist_id,0), COALESCE(album_id,0))
DO UPDATE SET created_at = now()`, userID, trackID)
	return err
}

func (s *PGStore) FavoritesRemove(userID int64, trackID int64) error {
	_, err := s.DB.Exec(
		`DELETE FROM user_favorites WHERE user_id = $1 AND kind = 'track' AND track_id = $2`,
		userID, trackID)
	return err
}

func (s *PGStore) FavoritesCount(userID int64) int {
	var n int
	_ = s.DB.QueryRow(
		`SELECT COUNT(*) FROM user_favorites WHERE user_id = $1 AND kind = 'track'`, userID).Scan(&n)
	return n
}

func (s *PGStore) FavoritesHas(userID int64, trackID int64) bool {
	var n int
	_ = s.DB.QueryRow(
		`SELECT 1 FROM user_favorites WHERE user_id = $1 AND kind = 'track' AND track_id = $2`,
		userID, trackID).Scan(&n)
	return n == 1
}

func (s *PGStore) FavArtistsList(userID int64) ([]FavArtist, error) {
	rows, err := s.DB.Query(`
SELECT a.name, f.position, f.created_at
FROM user_favorites f
JOIN artists a ON a.id = f.artist_id
WHERE f.user_id = $1 AND f.kind = 'artist'
ORDER BY f.position ASC, f.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavArtist
	for rows.Next() {
		var a FavArtist
		if err := rows.Scan(&a.Artist, &a.Position, &a.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// upsertArtistID возвращает id артиста по имени, создавая строку в artists
// при необходимости (ключ name_norm стабилен между rescan).
func (s *PGStore) upsertArtistID(artist string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`
INSERT INTO artists (name, name_norm) VALUES ($1, $2)
ON CONFLICT (name_norm) DO UPDATE SET name = excluded.name
RETURNING id`, artist, pgNameNorm(artist)).Scan(&id)
	return id, err
}

func (s *PGStore) FavArtistAdd(userID int64, artist string) error {
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return fmt.Errorf("artist required")
	}
	artistID, err := s.upsertArtistID(artist)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`
INSERT INTO user_favorites(user_id, kind, artist_id, position, created_at)
VALUES ($1,'artist',$2, COALESCE((SELECT MAX(position) FROM user_favorites
                                  WHERE user_id = $1 AND kind = 'artist'),0)+1, now())
ON CONFLICT (user_id, kind, COALESCE(track_id,0), COALESCE(artist_id,0), COALESCE(album_id,0))
DO UPDATE SET created_at = now()`, userID, artistID)
	return err
}

func (s *PGStore) FavArtistRemove(userID int64, artist string) error {
	_, err := s.DB.Exec(`
DELETE FROM user_favorites f
USING artists a
WHERE f.user_id = $1 AND f.kind = 'artist' AND f.artist_id = a.id AND a.name_norm = $2`,
		userID, pgNameNorm(artist))
	return err
}

func (s *PGStore) FavArtistHas(userID int64, artist string) bool {
	var n int
	_ = s.DB.QueryRow(`
SELECT 1 FROM user_favorites f
JOIN artists a ON a.id = f.artist_id
WHERE f.user_id = $1 AND f.kind = 'artist' AND a.name_norm = $2`,
		userID, pgNameNorm(artist)).Scan(&n)
	return n == 1
}

func (s *PGStore) FavArtistCount(userID int64) int {
	var n int
	_ = s.DB.QueryRow(
		`SELECT COUNT(*) FROM user_favorites WHERE user_id = $1 AND kind = 'artist'`, userID).Scan(&n)
	return n
}

func (s *PGStore) FavAlbumsList(userID int64) ([]FavAlbum, error) {
	rows, err := s.DB.Query(`
SELECT a.name, al.title, f.position, f.created_at
FROM user_favorites f
JOIN albums al ON al.id = f.album_id
JOIN artists a ON a.id = al.artist_id
WHERE f.user_id = $1 AND f.kind = 'album'
ORDER BY f.position ASC, f.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavAlbum
	for rows.Next() {
		var a FavAlbum
		if err := rows.Scan(&a.Artist, &a.Album, &a.Position, &a.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PGStore) FavAlbumAdd(userID int64, artist, album string) error {
	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	if album == "" {
		return fmt.Errorf("album required")
	}
	var artistID int64
	var err error
	if artist != "" {
		if artistID, err = s.upsertArtistID(artist); err != nil {
			return err
		}
	}
	var albumID int64
	err = s.DB.QueryRow(`
INSERT INTO albums (artist_id, title, title_norm)
VALUES ($1, $2, $3)
ON CONFLICT (artist_id, title_norm) DO UPDATE SET title = excluded.title
RETURNING id`, artistID, album, pgNameNorm(album)).Scan(&albumID)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`
INSERT INTO user_favorites(user_id, kind, album_id, position, created_at)
VALUES ($1,'album',$2, COALESCE((SELECT MAX(position) FROM user_favorites
                                  WHERE user_id = $1 AND kind = 'album'),0)+1, now())
ON CONFLICT (user_id, kind, COALESCE(track_id,0), COALESCE(artist_id,0), COALESCE(album_id,0))
DO UPDATE SET created_at = now()`, userID, albumID)
	return err
}

func (s *PGStore) FavAlbumRemove(userID int64, artist, album string) error {
	_, err := s.DB.Exec(`
DELETE FROM user_favorites f
USING artists a, albums al
WHERE f.user_id = $1 AND f.kind = 'album' AND f.album_id = al.id
  AND al.artist_id = a.id AND al.title_norm = $2
  AND a.name_norm = $3`, userID, pgNameNorm(album), pgNameNorm(artist))
	return err
}

func (s *PGStore) FavAlbumHas(userID int64, artist, album string) bool {
	var n int
	_ = s.DB.QueryRow(`
SELECT 1 FROM user_favorites f
JOIN albums al ON al.id = f.album_id
JOIN artists a ON a.id = al.artist_id
WHERE f.user_id = $1 AND f.kind = 'album'
  AND al.title_norm = $2 AND a.name_norm = $3`,
		userID, pgNameNorm(album), pgNameNorm(artist)).Scan(&n)
	return n == 1
}

func (s *PGStore) FavAlbumCount(userID int64) int {
	var n int
	_ = s.DB.QueryRow(
		`SELECT COUNT(*) FROM user_favorites WHERE user_id = $1 AND kind = 'album'`, userID).Scan(&n)
	return n
}

func (s *PGStore) LatestPlaylist(userID int64, kind string) (*Playlist, error) {
	var pl Playlist
	err := s.DB.QueryRow(`
SELECT id, kind, name, created_at FROM playlists
WHERE kind = $1 AND owner_user_id = $2 ORDER BY id DESC LIMIT 1`,
		kind, userID).Scan(&pl.ID, &pl.Kind, &pl.Name, &pl.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tracks, err := s.playlistTracks(`
SELECT pt.position, pt.track_id, COALESCE(t.artist,''), COALESCE(t.title,''),
       COALESCE(t.duration,0), COALESCE(pt.explanation,'')
FROM playlist_tracks pt
JOIN tracks t ON t.id = pt.track_id
WHERE pt.playlist_id = $1
ORDER BY pt.position`, pl.ID)
	if err != nil {
		return nil, err
	}
	pl.Tracks = tracks
	return &pl, nil
}

func (s *PGStore) ListDiscoverTips(userID int64, kind string, limit int) ([]DiscoverTip, error) {
	if limit < 1 {
		limit = 20
	}
	q := `
SELECT id, kind, COALESCE(artist,''), COALESCE(album,''), score,
       track_ids_json::text, COALESCE(explanation,''), created_at
FROM discover_tips WHERE user_id = $1`
	args := []any{userID}
	if kind != "" {
		q += ` AND kind = $2`
		args = append(args, kind)
	}
	q += ` ORDER BY score DESC, id DESC`
	if kind != "" {
		q += ` LIMIT $3`
	} else {
		q += ` LIMIT $2`
	}
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscoverTip
	for rows.Next() {
		var t DiscoverTip
		var idsJSON string
		if err := rows.Scan(&t.ID, &t.Kind, &t.Artist, &t.Album, &t.Score, &idsJSON, &t.Explanation, &t.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(idsJSON), &t.TrackIDs)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) playlistTracks(query string, args ...any) ([]PlaylistTrack, error) {
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlaylistTrack
	for rows.Next() {
		var t PlaylistTrack
		if err := rows.Scan(&t.Position, &t.TrackID, &t.Artist, &t.Title, &t.Duration, &t.Explanation); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
