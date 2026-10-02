package db

import (
	"fmt"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// PGStore: user-scoped медиа-аннотации Subsonic (F3.3, GitLab #25).
// scrobble → listening_history + user_track_stats (как /api/events
// track_end с reason=completed); star/unstar → user_favorites;
// setRating → user_ratings (обе таблицы — схема 00001, миграций не нужно).

var _ subsonic.Library = (*PGStore)(nil)

// favoriteColumn — целевая колонка kind в user_favorites/user_ratings.
var favoriteColumn = map[string]string{
	"track": "track_id", "album": "album_id", "artist": "artist_id",
}

// SubsonicScrobble — одно прослушивание: строка listening_history
// (action=track_end, source=subsonic) + completed в user_track_stats,
// чтобы frequent-списки видели и subsonic-прослушивания.
func (s *PGStore) SubsonicScrobble(userID, trackID int64, at time.Time) error {
	at = at.UTC()
	if _, err := s.DB.Exec(`
INSERT INTO listening_history(
  user_id, track_id, ts, source, action, daypart, weekday, reason
) VALUES ($1,$2,$3,'subsonic','track_end',$4,$5,'completed')`,
		userID, trackID, at, dayPart(at.Hour()), mondayZeroWeekday(at.Weekday())); err != nil {
		return fmt.Errorf("pg: subsonic scrobble: %w", err)
	}
	return s.BumpRecStats(userID, trackID, 0, 0, 1)
}

// SubsonicFavoriteSet — star/unstar (idempotent).
func (s *PGStore) SubsonicFavoriteSet(userID int64, kind string, id int64, starred bool) error {
	col, ok := favoriteColumn[kind]
	if !ok {
		return fmt.Errorf("pg: subsonic favorite kind %q", kind)
	}
	if !starred {
		_, err := s.DB.Exec(
			`DELETE FROM user_favorites WHERE user_id=$1 AND kind=$2 AND `+col+`=$3`,
			userID, kind, id)
		return err
	}
	_, err := s.DB.Exec(`
INSERT INTO user_favorites(user_id, kind, `+col+`)
VALUES ($1,$2,$3)
ON CONFLICT DO NOTHING`,
		userID, kind, id)
	return err
}

// SubsonicRatingSet — рейтинг 1–5 (0 = удалить).
func (s *PGStore) SubsonicRatingSet(userID int64, kind string, id int64, rating int) error {
	col, ok := favoriteColumn[kind]
	if !ok {
		return fmt.Errorf("pg: subsonic rating kind %q", kind)
	}
	if rating <= 0 {
		_, err := s.DB.Exec(
			`DELETE FROM user_ratings WHERE user_id=$1 AND kind=$2 AND `+col+`=$3`,
			userID, kind, id)
		return err
	}
	_, err := s.DB.Exec(`
INSERT INTO user_ratings(user_id, kind, `+col+`, value)
VALUES ($1,$2,$3,$4)
ON CONFLICT (user_id, kind, COALESCE(track_id,0), COALESCE(artist_id,0), COALESCE(album_id,0))
DO UPDATE SET value = excluded.value`,
		userID, kind, id, rating)
	return err
}

// SubsonicStarredSongs — избранные треки пользователя.
func (s *PGStore) SubsonicStarredSongs(userID int64) ([]subsonic.SongRow, error) {
	rows, err := s.DB.Query(subsonicSongSelect+
		`JOIN user_favorites uf ON uf.kind = 'track' AND uf.track_id = t.id AND uf.user_id = $1
WHERE t.is_active AND t.is_duplicate_of IS NULL
ORDER BY uf.created_at DESC, t.id`, userID)
	if err != nil {
		return nil, err
	}
	return scanSubsonicSongs(rows)
}

// SubsonicStarredAlbums — избранные альбомы (свежие звёзды первыми).
func (s *PGStore) SubsonicStarredAlbums(userID int64) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+
		`JOIN user_favorites uf ON uf.kind = 'album' AND uf.album_id = al.id AND uf.user_id = $1`+
		subsonicAlbumGroup+`, uf.created_at
ORDER BY uf.created_at DESC, al.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicStarredArtists — избранные артисты.
func (s *PGStore) SubsonicStarredArtists(userID int64) ([]subsonic.ArtistRow, error) {
	rows, err := s.DB.Query(`
SELECT a.id, a.name, a.name_norm, COUNT(al.id)
FROM artists a
JOIN user_favorites uf ON uf.kind = 'artist' AND uf.artist_id = a.id AND uf.user_id = $1
LEFT JOIN albums al ON al.artist_id = a.id
GROUP BY a.id, uf.created_at
ORDER BY uf.created_at DESC, a.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subsonic.ArtistRow
	for rows.Next() {
		var a subsonic.ArtistRow
		if err := rows.Scan(&a.ID, &a.Name, &a.NameNorm, &a.AlbumCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SubsonicFrequentAlbums — getAlbumList type=frequent: альбомы пользователя
// по SUM(user_track_stats.completed) (сабскриб scrobble/track_end её
// инкрементирует); без прослушиваний — пусто.
func (s *PGStore) SubsonicFrequentAlbums(userID int64, size, offset int) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(`
WITH plays AS (
  SELECT t.album_id AS album_id, SUM(uts.completed)::int AS plays
  FROM user_track_stats uts
  JOIN tracks t ON t.id = uts.track_id AND t.is_active AND t.is_duplicate_of IS NULL
  WHERE uts.user_id = $1
  GROUP BY t.album_id
  HAVING SUM(uts.completed) > 0
)
`+subsonicAlbumColumns+subsonicAlbumSelect+
		`JOIN plays p ON p.album_id = al.id`+
		subsonicAlbumGroup+`, p.plays
ORDER BY p.plays DESC, al.title_norm
LIMIT $2 OFFSET $3`, userID, size, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}
