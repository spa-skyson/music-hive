package db

func (s *PGStore) WeeklyMetrics(userID int64) (Metrics, error) {
	var m Metrics
	err := s.DB.QueryRow(`
SELECT
  COALESCE(SUM(CASE WHEN action IN ('track_end','finish','skip','like') THEN 1 ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN action='skip' OR (action='track_end' AND reason='skipped') THEN 1 ELSE 0 END),0),
  COALESCE(SUM(CASE WHEN action='finish' OR (action='track_end' AND reason='completed') THEN 1 ELSE 0 END),0)
FROM listening_history
WHERE user_id = $1 AND ts >= now() - interval '7 days'`, userID).Scan(&m.Listens7d, &m.Skips7d, &m.Completes7d)
	if err != nil {
		return m, err
	}
	if m.Listens7d > 0 {
		m.SkipRate7d = float64(m.Skips7d) / float64(m.Listens7d)
	}
	_ = s.DB.QueryRow(`
SELECT COUNT(DISTINCT t.artist)
FROM listening_history h
JOIN tracks t ON t.id = h.track_id
WHERE h.user_id = $1 AND h.ts >= now() - interval '7 days'`, userID).Scan(&m.UniqueArtists7d)
	err = s.DB.QueryRow(`
SELECT
  COALESCE(SUM(CASE WHEN i.explore = 1 THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN i.explore = 0 THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN i.explore = 1 AND EXISTS (
    SELECT 1 FROM listening_history h
    WHERE h.user_id = i.user_id AND h.session_id = i.session_id AND h.track_id = i.track_id
      AND h.action = 'track_end' AND h.reason = 'skipped'
      AND COALESCE(h.listened_sec,0) < 0.3 * COALESCE(h.duration_sec,1)
      AND h.ts >= i.shown_at
  ) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN i.explore = 0 AND EXISTS (
    SELECT 1 FROM listening_history h
    WHERE h.user_id = i.user_id AND h.session_id = i.session_id AND h.track_id = i.track_id
      AND h.action = 'track_end' AND h.reason = 'skipped'
      AND COALESCE(h.listened_sec,0) < 0.3 * COALESCE(h.duration_sec,1)
      AND h.ts >= i.shown_at
  ) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN i.explore = 1 AND EXISTS (
    SELECT 1 FROM listening_history h
    WHERE h.user_id = i.user_id AND h.session_id = i.session_id AND h.track_id = i.track_id
      AND h.action = 'track_end'
      AND (h.reason = 'completed' OR COALESCE(h.listened_sec,0) >= 0.8 * COALESCE(h.duration_sec,1))
      AND h.ts >= i.shown_at
  ) THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN i.explore = 0 AND EXISTS (
    SELECT 1 FROM listening_history h
    WHERE h.user_id = i.user_id AND h.session_id = i.session_id AND h.track_id = i.track_id
      AND h.action = 'track_end'
      AND (h.reason = 'completed' OR COALESCE(h.listened_sec,0) >= 0.8 * COALESCE(h.duration_sec,1))
      AND h.ts >= i.shown_at
  ) THEN 1 ELSE 0 END), 0)
FROM recommendation_impressions i
WHERE i.user_id = $1 AND i.shown_at >= now() - interval '7 days'`, userID).Scan(
		&m.ExploreShown, &m.ExploitShown, &m.ExploreSkips, &m.ExploitSkips,
		&m.ExploreComplete, &m.ExploitComplete,
	)
	if err != nil {
		return m, err
	}
	if m.ExploreShown > 0 {
		m.ExploreSkipRate = float64(m.ExploreSkips) / float64(m.ExploreShown)
		m.ExploreCompRate = float64(m.ExploreComplete) / float64(m.ExploreShown)
	}
	if m.ExploitShown > 0 {
		m.ExploitSkipRate = float64(m.ExploitSkips) / float64(m.ExploitShown)
		m.ExploitCompRate = float64(m.ExploitComplete) / float64(m.ExploitShown)
	}
	return m, nil
}
