package db

import (
	"database/sql"
	"fmt"
	"strconv"

	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// PGStore: read-only каталог для Subsonic browse/списков (F3.2, GitLab #24).
// Каталог общий — без user_id; треки everywhere фильтруются как в остальном
// каталоге: is_active AND is_duplicate_of IS NULL.

var _ subsonic.Catalog = (*PGStore)(nil)

// subsonicAlbumColumns/Select/Group — альбом с агрегатами по активным трекам
// (songCount, duration) и именем артиста; общий SELECT для всех album-методов.
const (
	subsonicAlbumColumns = `SELECT al.id, al.artist_id, ar.name, al.title,
       COALESCE(al.year, 0), COALESCE(al.genre, ''),
       COUNT(t.id), COALESCE(SUM(t.duration), 0)::int,
       al.created_at
`
	subsonicAlbumSelect = `FROM albums al
JOIN artists ar ON ar.id = al.artist_id
LEFT JOIN tracks t ON t.album_id = al.id AND t.is_active AND t.is_duplicate_of IS NULL
`
	subsonicAlbumGroup = ` GROUP BY al.id, ar.id`
)

// subsonicSongSelect — трек для child-элементов.
const subsonicSongSelect = `SELECT t.id, COALESCE(t.album_id, 0), COALESCE(t.artist_id, 0),
       COALESCE(t.title, ''), COALESCE(t.artist, ''), COALESCE(t.album, ''),
       COALESCE(t.track_number, 0), COALESCE(t.year, 0), COALESCE(t.duration, 0)::int,
       COALESCE(t.file_size, 0), COALESCE(t.bitrate, 0), COALESCE(t.disc_number, 0),
       t.path, t.created_at
FROM tracks t
`

func scanSubsonicAlbums(rows *sql.Rows) ([]subsonic.AlbumRow, error) {
	defer rows.Close()
	var out []subsonic.AlbumRow
	for rows.Next() {
		var a subsonic.AlbumRow
		if err := rows.Scan(&a.ID, &a.ArtistID, &a.Artist, &a.Title,
			&a.Year, &a.Genre, &a.SongCount, &a.Duration, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanSubsonicSongs(rows *sql.Rows) ([]subsonic.SongRow, error) {
	defer rows.Close()
	var out []subsonic.SongRow
	for rows.Next() {
		var s subsonic.SongRow
		if err := rows.Scan(&s.ID, &s.AlbumID, &s.ArtistID,
			&s.Title, &s.Artist, &s.Album,
			&s.Track, &s.Year, &s.Duration,
			&s.Size, &s.Bitrate, &s.DiscNumber,
			&s.Path, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SubsonicArtists — все артисты с albumCount, отсортированы по name_norm
// (порядок внутри index-блока getArtists/getIndexes).
func (s *PGStore) SubsonicArtists() ([]subsonic.ArtistRow, error) {
	rows, err := s.DB.Query(`
SELECT a.id, a.name, a.name_norm, COUNT(al.id)
FROM artists a
LEFT JOIN albums al ON al.artist_id = a.id
GROUP BY a.id
ORDER BY a.name_norm`)
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

// SubsonicArtist — один артист (70-кейс getArtist решает вызывающий).
func (s *PGStore) SubsonicArtist(id int64) (subsonic.ArtistRow, bool, error) {
	var a subsonic.ArtistRow
	err := s.DB.QueryRow(`
SELECT a.id, a.name, a.name_norm, COUNT(al.id)
FROM artists a
LEFT JOIN albums al ON al.artist_id = a.id
WHERE a.id = $1
GROUP BY a.id`, id).Scan(&a.ID, &a.Name, &a.NameNorm, &a.AlbumCount)
	if err == sql.ErrNoRows {
		return subsonic.ArtistRow{}, false, nil
	}
	if err != nil {
		return subsonic.ArtistRow{}, false, fmt.Errorf("pg: subsonic artist: %w", err)
	}
	return a, true, nil
}

// SubsonicArtistAlbums — альбомы артиста по году выпуска.
func (s *PGStore) SubsonicArtistAlbums(artistID int64) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+
		`WHERE al.artist_id = $1`+subsonicAlbumGroup+
		` ORDER BY al.year NULLS LAST, al.title_norm`, artistID)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicAlbum — один альбом с именем артиста.
func (s *PGStore) SubsonicAlbum(id int64) (subsonic.AlbumRow, bool, error) {
	var a subsonic.AlbumRow
	err := s.DB.QueryRow(subsonicAlbumColumns+subsonicAlbumSelect+
		`WHERE al.id = $1`+subsonicAlbumGroup, id).
		Scan(&a.ID, &a.ArtistID, &a.Artist, &a.Title,
			&a.Year, &a.Genre, &a.SongCount, &a.Duration, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return subsonic.AlbumRow{}, false, nil
	}
	if err != nil {
		return subsonic.AlbumRow{}, false, fmt.Errorf("pg: subsonic album: %w", err)
	}
	return a, true, nil
}

// SubsonicAlbumSongs — треки альбома по disc/track-номеру.
func (s *PGStore) SubsonicAlbumSongs(albumID int64) ([]subsonic.SongRow, error) {
	rows, err := s.DB.Query(subsonicSongSelect+
		`WHERE t.is_active AND t.is_duplicate_of IS NULL AND t.album_id = $1
ORDER BY t.disc_number NULLS LAST, t.track_number NULLS LAST, t.id`, albumID)
	if err != nil {
		return nil, err
	}
	return scanSubsonicSongs(rows)
}

// SubsonicSong — один трек.
func (s *PGStore) SubsonicSong(id int64) (subsonic.SongRow, bool, error) {
	var s2 subsonic.SongRow
	err := s.DB.QueryRow(subsonicSongSelect+
		`WHERE t.is_active AND t.is_duplicate_of IS NULL AND t.id = $1`, id).
		Scan(&s2.ID, &s2.AlbumID, &s2.ArtistID,
			&s2.Title, &s2.Artist, &s2.Album,
			&s2.Track, &s2.Year, &s2.Duration,
			&s2.Size, &s2.Bitrate, &s2.DiscNumber,
			&s2.Path, &s2.CreatedAt)
	if err == sql.ErrNoRows {
		return subsonic.SongRow{}, false, nil
	}
	if err != nil {
		return subsonic.SongRow{}, false, fmt.Errorf("pg: subsonic song: %w", err)
	}
	return s2, true, nil
}

// SubsonicAlbumList — типы random/newest/alphabeticalByName/
// alphabeticalArtist/byYear/byGenre; recent/frequent/highest/starred
// отсеиваются хендлером (нет данных, см. album_lists.go).
func (s *PGStore) SubsonicAlbumList(q subsonic.AlbumListQuery) ([]subsonic.AlbumRow, error) {
	var args []any
	ph := func(v any) string { // нумерация $n по мере сборки запроса
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where, order := "", ""
	switch q.Type {
	case "random":
		order = ` ORDER BY random()`
	case "newest":
		order = ` ORDER BY al.created_at DESC, al.id DESC`
	case "alphabeticalByName":
		order = ` ORDER BY al.title_norm, ar.name_norm`
	case "alphabeticalArtist":
		order = ` ORDER BY ar.name_norm, al.title_norm`
	case "byYear":
		lo, hi := q.FromYear, q.ToYear
		if lo > hi { // fromYear > toYear — обратный порядок (спецификация)
			lo, hi = hi, lo
			order = ` ORDER BY al.year DESC, al.title_norm`
		} else {
			order = ` ORDER BY al.year, al.title_norm`
		}
		where = ` WHERE al.year BETWEEN ` + ph(lo) + ` AND ` + ph(hi)
	case "byGenre":
		where = ` WHERE al.genre = ` + ph(q.Genre)
	default:
		return nil, fmt.Errorf("pg: subsonic album list type %q", q.Type)
	}
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+where+
		subsonicAlbumGroup+order+` LIMIT `+ph(q.Size)+` OFFSET `+ph(q.Offset), args...)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicRandomSongs — случайные треки (ORDER BY random()).
func (s *PGStore) SubsonicRandomSongs(size int) ([]subsonic.SongRow, error) {
	rows, err := s.DB.Query(subsonicSongSelect+
		`WHERE t.is_active AND t.is_duplicate_of IS NULL
ORDER BY random()
LIMIT $1`, size)
	if err != nil {
		return nil, err
	}
	return scanSubsonicSongs(rows)
}

// SubsonicSongsByGenre — треки жанра по имени из genres (значение клиенты
// берут из getGenres), стабильный порядок для пагинации.
func (s *PGStore) SubsonicSongsByGenre(genre string, count, offset int) ([]subsonic.SongRow, error) {
	rows, err := s.DB.Query(subsonicSongSelect+
		`JOIN track_genres tg ON tg.track_id = t.id
JOIN genres g ON g.id = tg.genre_id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND g.name = $1
ORDER BY t.id
LIMIT $2 OFFSET $3`, genre, count, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicSongs(rows)
}

// SubsonicGenres — жанр + счётчики (songCount по track_genres, albumCount
// по мажоритарному albums.genre).
func (s *PGStore) SubsonicGenres() ([]subsonic.GenreRow, error) {
	rows, err := s.DB.Query(`
SELECT g.name,
       (SELECT COUNT(*) FROM track_genres tg WHERE tg.genre_id = g.id),
       (SELECT COUNT(*) FROM albums a WHERE a.genre = g.name)
FROM genres g
ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subsonic.GenreRow
	for rows.Next() {
		var g subsonic.GenreRow
		if err := rows.Scan(&g.Name, &g.SongCount, &g.AlbumCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- F3.3 (#25)

// ilike — подстрока ILIKE (поиск search2/search3).
func ilike(sub string) string { return "%" + sub + "%" }

// SubsonicSearchArtists — артисты по подстроке имени.
func (s *PGStore) SubsonicSearchArtists(query string, count, offset int) ([]subsonic.ArtistRow, error) {
	rows, err := s.DB.Query(`
SELECT a.id, a.name, a.name_norm, COUNT(al.id)
FROM artists a
LEFT JOIN albums al ON al.artist_id = a.id
WHERE a.name ILIKE $1
GROUP BY a.id
ORDER BY a.name_norm
LIMIT $2 OFFSET $3`, ilike(query), count, offset)
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

// SubsonicSearchAlbums — альбомы по подстроке названия или имени артиста.
func (s *PGStore) SubsonicSearchAlbums(query string, count, offset int) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+
		`WHERE (al.title ILIKE $1 OR ar.name ILIKE $1)`+subsonicAlbumGroup+
		` ORDER BY al.title_norm, ar.name_norm
LIMIT $2 OFFSET $3`, ilike(query), count, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicSearchSongs — треки по подстроке title/artist/album.
func (s *PGStore) SubsonicSearchSongs(query string, count, offset int) ([]subsonic.SongRow, error) {
	rows, err := s.DB.Query(subsonicSongSelect+
		`WHERE t.is_active AND t.is_duplicate_of IS NULL
  AND (t.title ILIKE $1 OR t.artist ILIKE $1 OR t.album ILIKE $1)
ORDER BY t.title, t.id
LIMIT $2 OFFSET $3`, ilike(query), count, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicSongs(rows)
}

// SubsonicRecentAlbums — getAlbumList type=recent: альбомы по самому
// свежему треку (max(tracks.created_at)) — добавленная музыка, а не
// прослушивание (решение #25, см. subsonic-api.md).
func (s *PGStore) SubsonicRecentAlbums(size, offset int) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+
		subsonicAlbumGroup+` ORDER BY MAX(t.created_at) DESC NULLS LAST, al.id DESC
LIMIT $1 OFFSET $2`, size, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicHighestAlbums — getAlbumList type=highest: средний user_ratings
// по всем пользователям (маппинг averageRating, subsonic-api.md §2).
// Рейтинги агрегируются в подзапросе — иначе LEFT JOIN размножил бы треки
// и завысил songCount/duration.
func (s *PGStore) SubsonicHighestAlbums(size, offset int) ([]subsonic.AlbumRow, error) {
	rows, err := s.DB.Query(subsonicAlbumColumns+subsonicAlbumSelect+
		`LEFT JOIN (SELECT album_id, AVG(value) AS rating
		            FROM user_ratings WHERE kind = 'album' GROUP BY album_id) ur
		      ON ur.album_id = al.id`+
		subsonicAlbumGroup+`
HAVING MAX(ur.rating) > 0
ORDER BY MAX(ur.rating) DESC, al.title_norm
LIMIT $1 OFFSET $2`, size, offset)
	if err != nil {
		return nil, err
	}
	return scanSubsonicAlbums(rows)
}

// SubsonicCoverTrack — resolve coverArt-ида в трек с artwork_path:
// mf → сам трек; al → cover_track_id альбома (иначе первый трек с
// artwork по disc/track); ar → первый альбом артиста (обложек артистов
// в схеме нет). found=false → код 70.
func (s *PGStore) SubsonicCoverTrack(kind string, id int64) (int64, string, bool, error) {
	query := map[string]string{
		"mf": `
SELECT t.id, t.artwork_path
FROM tracks t
WHERE t.id = $1 AND t.is_active AND t.is_duplicate_of IS NULL
  AND COALESCE(t.artwork_path, '') <> ''`,
		"al": `
SELECT t.id, t.artwork_path
FROM tracks t
JOIN albums al ON al.id = t.album_id
WHERE al.id = $1 AND t.is_active AND t.is_duplicate_of IS NULL
  AND COALESCE(t.artwork_path, '') <> ''
ORDER BY (t.id = al.cover_track_id) DESC,
         t.disc_number NULLS LAST, t.track_number NULLS LAST, t.id
LIMIT 1`,
		"ar": `
SELECT t.id, t.artwork_path
FROM tracks t
JOIN albums al ON al.id = t.album_id
WHERE al.artist_id = $1 AND t.is_active AND t.is_duplicate_of IS NULL
  AND COALESCE(t.artwork_path, '') <> ''
ORDER BY al.year NULLS LAST, al.title_norm,
         t.disc_number NULLS LAST, t.track_number NULLS LAST, t.id
LIMIT 1`,
	}[kind]
	if query == "" {
		return 0, "", false, fmt.Errorf("pg: subsonic cover kind %q", kind)
	}
	var trackID int64
	var path string
	err := s.DB.QueryRow(query, id).Scan(&trackID, &path)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("pg: subsonic cover: %w", err)
	}
	return trackID, path, true, nil
}

// SubsonicLyricsForSong — legacy getLyrics: точный ILIKE-матч artist+title,
// приоритет готовых текстов.
func (s *PGStore) SubsonicLyricsForSong(artist, title string) (subsonic.LyricsRow, bool, error) {
	var row subsonic.LyricsRow
	err := s.DB.QueryRow(`
SELECT t.artist, t.title, ly.plain_lyrics, ly.synced_lyrics
FROM lyrics ly
JOIN tracks t ON t.id = ly.track_id
WHERE t.artist ILIKE $1 AND t.title ILIKE $2
  AND (ly.plain_lyrics <> '' OR ly.synced_lyrics <> '')
ORDER BY (ly.status = 'ready') DESC, t.id
LIMIT 1`, artist, title).
		Scan(&row.Artist, &row.Title, &row.Plain, &row.Synced)
	if err == sql.ErrNoRows {
		return subsonic.LyricsRow{}, false, nil
	}
	if err != nil {
		return subsonic.LyricsRow{}, false, fmt.Errorf("pg: subsonic lyrics: %w", err)
	}
	return row, true, nil
}

// SubsonicLyricsForTrack — getLyricsBySongId.
func (s *PGStore) SubsonicLyricsForTrack(trackID int64) (subsonic.LyricsRow, bool, error) {
	var row subsonic.LyricsRow
	err := s.DB.QueryRow(`
SELECT t.artist, t.title, ly.plain_lyrics, ly.synced_lyrics
FROM lyrics ly
JOIN tracks t ON t.id = ly.track_id
WHERE ly.track_id = $1
  AND (ly.plain_lyrics <> '' OR ly.synced_lyrics <> '')`, trackID).
		Scan(&row.Artist, &row.Title, &row.Plain, &row.Synced)
	if err == sql.ErrNoRows {
		return subsonic.LyricsRow{}, false, nil
	}
	if err != nil {
		return subsonic.LyricsRow{}, false, fmt.Errorf("pg: subsonic lyrics track: %w", err)
	}
	return row, true, nil
}
