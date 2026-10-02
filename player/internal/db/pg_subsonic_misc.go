package db

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// PGStore: play-queue/bookmarks/radio/scan окна Subsonic (F3.4, #26) —
// поверх существующих таблиц (play_sessions, user_listen_later,
// radio_shares, jobs/tracks) без миграций.

var (
	_ subsonic.QueueStore    = (*PGStore)(nil)
	_ subsonic.BookmarkStore = (*PGStore)(nil)
	_ subsonic.RadioStore    = (*PGStore)(nil)
	_ subsonic.ScanStore     = (*PGStore)(nil)
)

// ---------------------------------------------------------------- play-queue

// subsonicQueueSessionID — фиксированный id строки play_sessions с
// сохранённой subsonic-очередью пользователя (одна очередь на юзера;
// PK play_sessions — id без user_id, поэтому суффикс).
func subsonicQueueSessionID(userID int64) string {
	return "subsonic-" + strconv.FormatInt(userID, 10)
}

// SubsonicSaveQueue — upsert subsonic-строки play_sessions: состав — JSON
// в queue_json, текущий трек — current_id, changed — updated_at.
func (s *PGStore) SubsonicSaveQueue(userID int64, q subsonic.SavedQueue) error {
	blob, err := subsonic.MarshalQueueJSON(q)
	if err != nil {
		return err
	}
	return s.UpsertPlaySession(PlaySessionRow{
		ID:        subsonicQueueSessionID(userID),
		UserID:    userID,
		Mode:      "subsonic",
		CurrentID: q.Current,
		QueueJSON: blob,
		UpdatedAt: q.Changed.UTC().Format(time.RFC3339Nano),
	})
}

// SubsonicLoadQueue — чтение сохранённой очереди (false — не сохранял).
func (s *PGStore) SubsonicLoadQueue(userID int64) (subsonic.SavedQueue, bool, error) {
	row, found, err := s.LoadPlaySession(userID, subsonicQueueSessionID(userID))
	if err != nil || !found {
		return subsonic.SavedQueue{}, found, err
	}
	q, err := subsonic.UnmarshalQueueJSON(row.QueueJSON)
	if err != nil {
		return subsonic.SavedQueue{}, false, fmt.Errorf("pg: subsonic queue: %w", err)
	}
	q.Current = row.CurrentID
	return q, true, nil
}

// SubsonicNowPlaying — сессии плеера, обновлённые за 10 минут (свои
// subsonic-строки очередей и пустые — не «играющие»).
func (s *PGStore) SubsonicNowPlaying() ([]subsonic.NowPlayingRow, error) {
	rows, err := s.DB.Query(`
SELECT u.username, p.current_id, p.updated_at
FROM play_sessions p
JOIN users u ON u.id = p.user_id
WHERE p.updated_at > now() - interval '10 minutes'
  AND p.current_id <> 0 AND p.mode <> 'subsonic'
ORDER BY p.updated_at DESC
LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("pg: subsonic now playing: %w", err)
	}
	defer rows.Close()
	var out []subsonic.NowPlayingRow
	for rows.Next() {
		var row subsonic.NowPlayingRow
		if err := rows.Scan(&row.Username, &row.TrackID, &row.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- bookmarks

// SubsonicBookmarks — «послушать позже» (позже первых; position — порядок
// списка, не мс — хендлер отдаёт 0).
func (s *PGStore) SubsonicBookmarks(userID int64) ([]subsonic.BookmarkRow, error) {
	rows, err := s.DB.Query(
		`SELECT track_id, added_at FROM user_listen_later WHERE user_id = $1
ORDER BY position ASC, added_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("pg: subsonic bookmarks: %w", err)
	}
	defer rows.Close()
	var out []subsonic.BookmarkRow
	for rows.Next() {
		var row subsonic.BookmarkRow
		if err := rows.Scan(&row.TrackID, &row.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *PGStore) SubsonicBookmarkAdd(userID, trackID int64) error {
	return s.LaterAdd(userID, trackID)
}

func (s *PGStore) SubsonicBookmarkRemove(userID, trackID int64) error {
	return s.LaterRemove(userID, trackID)
}

// ---------------------------------------------------------------- radio

// SubsonicRadioList — активные share-радио пользователя.
func (s *PGStore) SubsonicRadioList(userID int64) ([]subsonic.RadioStationRow, error) {
	shares, err := s.ListRadioShares(userID, false)
	if err != nil {
		return nil, err
	}
	out := make([]subsonic.RadioStationRow, 0, len(shares))
	for _, sh := range shares {
		out = append(out, subsonic.RadioStationRow{Token: sh.Token, Name: sh.Name, OwnerID: userID})
	}
	return out, nil
}

func (s *PGStore) SubsonicRadioCreate(userID int64, token, name string) (subsonic.RadioStationRow, error) {
	if _, err := s.CreateRadioShare(userID, token, name); err != nil {
		return subsonic.RadioStationRow{}, err
	}
	return subsonic.RadioStationRow{Token: token, Name: name, OwnerID: userID}, nil
}

// SubsonicRadioGet — активная станция по токену (отозванная = удалённая:
// 70 на повторный delete, как у Navidrome).
func (s *PGStore) SubsonicRadioGet(token string) (subsonic.RadioStationRow, bool, error) {
	var row subsonic.RadioStationRow
	err := s.DB.QueryRow(
		`SELECT token, name, user_id FROM radio_shares WHERE token = $1 AND revoked_at IS NULL`, token).
		Scan(&row.Token, &row.Name, &row.OwnerID)
	if err == sql.ErrNoRows {
		return subsonic.RadioStationRow{}, false, nil
	}
	if err != nil {
		return subsonic.RadioStationRow{}, false, fmt.Errorf("pg: subsonic radio get: %w", err)
	}
	return row, true, nil
}

func (s *PGStore) SubsonicRadioRename(token, name string) error {
	_, err := s.DB.Exec(`UPDATE radio_shares SET name = $2 WHERE token = $1`, token, name)
	if err != nil {
		return fmt.Errorf("pg: subsonic radio rename: %w", err)
	}
	return nil
}

func (s *PGStore) SubsonicRadioRevoke(token string) error {
	if _, err := s.DB.Exec(
		`UPDATE radio_shares SET revoked_at = now() WHERE token = $1 AND revoked_at IS NULL`, token); err != nil {
		return fmt.Errorf("pg: subsonic radio revoke: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- scan

// SubsonicScanStatus — count активных треков каталога + scanning по
// pending/running джобам scan/full_rescan (полный и быстрый рескан).
func (s *PGStore) SubsonicScanStatus() (int64, bool, error) {
	var count int64
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM tracks WHERE is_active AND is_duplicate_of IS NULL`).
		Scan(&count); err != nil {
		return 0, false, fmt.Errorf("pg: subsonic scan count: %w", err)
	}
	var scanning bool
	if err := s.DB.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM jobs
  WHERE kind IN ('scan', 'full_rescan') AND status IN ('pending', 'running')
)`).Scan(&scanning); err != nil {
		return 0, false, fmt.Errorf("pg: subsonic scan status: %w", err)
	}
	return count, scanning, nil
}
