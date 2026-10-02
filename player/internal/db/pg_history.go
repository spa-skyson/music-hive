package db

import (
	"time"
)

// RecentTrackIDs возвращает distinct track_id, слушавшиеся за последние
// hours часов (свежие первыми).
func (s *PGStore) RecentTrackIDs(userID int64, hours int, limit int) ([]int64, error) {
	if hours < 1 {
		hours = 24
	}
	if limit < 1 {
		limit = 40
	}
	rows, err := s.DB.Query(`
SELECT track_id FROM (
  SELECT track_id, MAX(ts) AS last_ts
  FROM listening_history
  WHERE user_id = $1 AND ts >= now() - make_interval(hours => $2)
  GROUP BY track_id
  ORDER BY last_ts DESC
  LIMIT $3
) recent`, userID, hours, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *PGStore) InsertListen(userID int64, trackID int64, action, source, sessionID, reason string,
	position, duration, listened *float64) (int64, error) {
	now := time.Now().UTC()
	var id int64
	err := s.DB.QueryRow(`
INSERT INTO listening_history(
  user_id, track_id, ts, source, action, daypart, weekday,
  position_sec, duration_sec, listened_sec, session_id, reason
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		userID, trackID, now, source, action, dayPart(now.Hour()),
		mondayZeroWeekday(now.Weekday()),
		position, duration, listened, nullStr(sessionID), nullStr(reason)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *PGStore) BumpTransition(fromID, toID int64, weight float64) error {
	_, err := s.DB.Exec(`
INSERT INTO transitions(from_id, to_id, weight, updated_at) VALUES ($1,$2,$3,now())
ON CONFLICT(from_id, to_id) DO UPDATE SET
  weight = transitions.weight + excluded.weight,
  updated_at = now()`, fromID, toID, weight)
	return err
}

func (s *PGStore) BumpRecStats(userID int64, trackID int64, shown, skipEarly, completed int) error {
	_, err := s.DB.Exec(`
INSERT INTO user_track_stats(user_id, track_id, shown, skipped_early, completed, updated_at)
VALUES ($1,$2,$3,$4,$5,now())
ON CONFLICT(user_id, track_id) DO UPDATE SET
  shown = user_track_stats.shown + excluded.shown,
  skipped_early = user_track_stats.skipped_early + excluded.skipped_early,
  completed = user_track_stats.completed + excluded.completed,
  updated_at = now()`,
		userID, trackID, shown, skipEarly, completed)
	return err
}

// InsertRecommendationImpressions записывает одну видимую очередь в транзакции.
func (s *PGStore) InsertRecommendationImpressions(userID int64, items []RecommendationImpression) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
INSERT INTO recommendation_impressions(
  user_id, session_id, track_id, position, score, cosine_taste, cosine_current,
  explore, new_boost, maturity, mode, shown_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now())`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	statsStmt, err := tx.Prepare(`
INSERT INTO user_track_stats(user_id, track_id, shown, skipped_early, completed, updated_at)
VALUES ($1,$2,1,0,0,now())
ON CONFLICT(user_id, track_id) DO UPDATE SET
  shown = user_track_stats.shown + 1,
  updated_at = now()`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer statsStmt.Close()
	for _, item := range items {
		if _, err := stmt.Exec(
			userID, item.SessionID, item.TrackID, item.Position, item.Score,
			item.CosineTaste, item.CosineCurrent, b2i(item.Explore), b2i(item.NewBoost),
			item.Maturity, item.Mode,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := statsStmt.Exec(userID, item.TrackID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
