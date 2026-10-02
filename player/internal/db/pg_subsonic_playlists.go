package db

import (
	"database/sql"
	"fmt"

	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// PGStore: плейлисты Subsonic (F3.4, GitLab #26) — таблицы playlists/
// playlist_tracks из схемы 00001; comment — в meta_json, changed =
// created (updated_at у playlists нет). Видимость getPlaylists — только
// свои (совместное использование публичных — #8); мутации чужого —
// (owner_user_id = userID OR isAdmin).

var _ subsonic.PlaylistStore = (*PGStore)(nil)

const (
	subsonicPlaylistColumns = `SELECT p.id, p.name, COALESCE(p.meta_json->>'comment',''),
       p.owner_user_id, u.username, p.visibility = 'public', p.created_at
`
	subsonicPlaylistFrom = `FROM playlists p
JOIN users u ON u.id = p.owner_user_id
`
)

// SubsonicPlaylists — свои плейлисты с счётчиками (kind не фильтруем:
// системные mix_* — тоже плейлисты пользователя).
func (s *PGStore) SubsonicPlaylists(userID int64) ([]subsonic.PlaylistRow, error) {
	rows, err := s.DB.Query(subsonicPlaylistColumns+`,
       COUNT(pt.track_id), COALESCE(SUM(t.duration), 0)::int
`+subsonicPlaylistFrom+`
LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
LEFT JOIN tracks t ON t.id = pt.track_id
WHERE p.owner_user_id = $1
GROUP BY p.id, u.id
ORDER BY p.id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("pg: subsonic playlists: %w", err)
	}
	defer rows.Close()
	var out []subsonic.PlaylistRow
	for rows.Next() {
		var p subsonic.PlaylistRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Comment,
			&p.OwnerID, &p.Owner, &p.Public, &p.CreatedAt,
			&p.SongCount, &p.Duration); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SubsonicPlaylist — плейлист с треками (без проверки владельца: доступ
// решает хендлер по OwnerID).
func (s *PGStore) SubsonicPlaylist(id int64) (subsonic.PlaylistRow, bool, error) {
	var p subsonic.PlaylistRow
	err := s.DB.QueryRow(subsonicPlaylistColumns+subsonicPlaylistFrom+`WHERE p.id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Comment, &p.OwnerID, &p.Owner, &p.Public, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return subsonic.PlaylistRow{}, false, nil
	}
	if err != nil {
		return subsonic.PlaylistRow{}, false, fmt.Errorf("pg: subsonic playlist: %w", err)
	}
	rows, err := s.DB.Query(subsonicSongSelect+`
JOIN playlist_tracks pt ON pt.track_id = t.id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND pt.playlist_id = $1
ORDER BY pt.position`, id)
	if err != nil {
		return subsonic.PlaylistRow{}, false, err
	}
	tracks, err := scanSubsonicSongs(rows)
	if err != nil {
		return subsonic.PlaylistRow{}, false, err
	}
	p.Tracks = tracks
	p.SongCount = len(tracks)
	for _, t := range tracks {
		p.Duration += t.Duration
	}
	return p, true, nil
}

// SubsonicPlaylistCreate — новый плейлист kind='user' с начальным составом.
func (s *PGStore) SubsonicPlaylistCreate(userID int64, name string, songIDs []int64) (subsonic.PlaylistRow, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return subsonic.PlaylistRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err := tx.QueryRow(
		`INSERT INTO playlists(owner_user_id, kind, name) VALUES ($1, 'user', $2) RETURNING id`,
		userID, name).Scan(&id); err != nil {
		return subsonic.PlaylistRow{}, fmt.Errorf("pg: subsonic playlist create: %w", err)
	}
	if err := subsonicInsertTracks(tx, id, songIDs); err != nil {
		return subsonic.PlaylistRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return subsonic.PlaylistRow{}, err
	}
	row, _, err := s.SubsonicPlaylist(id)
	return row, err
}

// SubsonicPlaylistReplace — полная замена состава (createPlaylist с
// playlistId); false = не найден или чужой.
func (s *PGStore) SubsonicPlaylistReplace(userID, id int64, songIDs []int64, isAdmin bool) (bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := subsonicClaimPlaylist(tx, userID, id, isAdmin)
	if err != nil || !ok {
		return ok, err
	}
	if _, err := tx.Exec(`DELETE FROM playlist_tracks WHERE playlist_id = $1`, id); err != nil {
		return false, err
	}
	if err := subsonicInsertTracks(tx, id, songIDs); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SubsonicPlaylistUpdate — частичное: name/comment + добавить/удалить по
// позициям; удаление и добавление применяются к текущему составу, позиции
// перенумеровываются заново. false = не найден или чужой.
func (s *PGStore) SubsonicPlaylistUpdate(userID, id int64, name, comment *string, addIDs []int64, removePositions []int, isAdmin bool) (bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := subsonicClaimPlaylist(tx, userID, id, isAdmin)
	if err != nil || !ok {
		return ok, err
	}
	if name != nil {
		if _, err := tx.Exec(`UPDATE playlists SET name = $2 WHERE id = $1`, id, *name); err != nil {
			return false, err
		}
	}
	if comment != nil {
		if _, err := tx.Exec(`
UPDATE playlists
SET meta_json = jsonb_set(COALESCE(meta_json, '{}'::jsonb), '{comment}', to_jsonb($2::text))
WHERE id = $1`, id, *comment); err != nil {
			return false, err
		}
	}
	rows, err := tx.Query(
		`SELECT track_id FROM playlist_tracks WHERE playlist_id = $1 ORDER BY position`, id)
	if err != nil {
		return false, err
	}
	var current []int64
	for rows.Next() {
		var trackID int64
		if err := rows.Scan(&trackID); err != nil {
			rows.Close()
			return false, err
		}
		current = append(current, trackID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	remove := make(map[int]bool, len(removePositions))
	for _, pos := range removePositions {
		remove[pos] = true
	}
	next := make([]int64, 0, len(current)+len(addIDs))
	for i, trackID := range current {
		if !remove[i] {
			next = append(next, trackID)
		}
	}
	next = append(next, addIDs...)
	if _, err := tx.Exec(`DELETE FROM playlist_tracks WHERE playlist_id = $1`, id); err != nil {
		return false, err
	}
	if err := subsonicInsertTracks(tx, id, next); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SubsonicPlaylistDelete — каскад чистит playlist_tracks; false = не
// найден или чужой.
func (s *PGStore) SubsonicPlaylistDelete(userID, id int64, isAdmin bool) (bool, error) {
	res, err := s.DB.Exec(
		`DELETE FROM playlists WHERE id = $1 AND (owner_user_id = $2 OR $3)`, id, userID, isAdmin)
	if err != nil {
		return false, fmt.Errorf("pg: subsonic playlist delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// subsonicClaimPlaylist — FOR UPDATE-блокировка плейлиста за владельцем
// (или админом) внутри tx; false = не найден/чужой.
func subsonicClaimPlaylist(tx *sql.Tx, userID, id int64, isAdmin bool) (bool, error) {
	var one int
	err := tx.QueryRow(
		`SELECT 1 FROM playlists WHERE id = $1 AND (owner_user_id = $2 OR $3) FOR UPDATE`,
		id, userID, isAdmin).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// subsonicInsertTracks — состав с последовательными позициями с 1.
func subsonicInsertTracks(tx *sql.Tx, playlistID int64, songIDs []int64) error {
	for i, trackID := range songIDs {
		if _, err := tx.Exec(
			`INSERT INTO playlist_tracks(playlist_id, position, track_id) VALUES ($1, $2, $3)`,
			playlistID, i+1, trackID); err != nil {
			return fmt.Errorf("pg: subsonic playlist tracks: %w", err)
		}
	}
	return nil
}
