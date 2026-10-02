package db

import "database/sql"

// Каталог: треки + векторная готовность по активной модели.
// LoadReadyTracks не тянет векторы: поиск — по pgvector
// (internal/index.PGIndex), матрица в RAM не нужна.

func (s *PGStore) LoadReadyTracks() ([]TrackRow, error) {
	_, dim, emb := s.ActiveModel()
	if emb == "" {
		return nil, nil // модель не активирована — векторного индекса нет
	}
	rows, err := s.DB.Query(`
SELECT t.id, t.path, COALESCE(t.title,''), COALESCE(t.artist,''), COALESCE(t.album,''),
       COALESCE(t.duration,0), COALESCE(t.file_md5,''), t.created_at,
       COALESCE(t.artwork_path,''), COALESCE(f.cluster_id,-1),
       NULL, $1::int,
       COALESCE(st.shown,0), COALESCE(st.skipped_early,0), COALESCE(st.completed,0)
FROM tracks t
JOIN `+emb+` e ON e.track_id = t.id AND e.status = 'ready'
LEFT JOIN track_audio_features f ON f.track_id = t.id
LEFT JOIN user_track_stats st ON st.track_id = t.id AND st.user_id = $2
WHERE t.is_active AND t.is_duplicate_of IS NULL
ORDER BY t.id`, dim, s.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackRow
	for rows.Next() {
		var tr TrackRow
		var embedding any // колонка-заглушка NULL на месте BLOB-вектора
		if err := rows.Scan(&tr.ID, &tr.Path, &tr.Title, &tr.Artist, &tr.Album,
			&tr.Duration, &tr.FileMD5, &tr.CreatedAt, &tr.ArtworkPath, &tr.ClusterID,
			&embedding, &tr.Dim,
			&tr.Shown, &tr.SkipEarly, &tr.Completed); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// ReadyTrackCount реализует Backend: count строк, которые вернёт
// LoadReadyTracks — готовность по активной модели emb-таблицы (не по
// аудио-фичам), точечный запрос по PK emb-таблицы.
func (s *PGStore) ReadyTrackCount() (int, error) {
	_, _, emb := s.ActiveModel()
	if emb == "" {
		return 0, nil // модель не активирована — векторного индекса нет
	}
	var n int
	err := s.DB.QueryRow(`
SELECT count(*) FROM tracks t
JOIN ` + emb + ` e ON e.track_id = t.id AND e.status = 'ready'
WHERE t.is_active AND t.is_duplicate_of IS NULL`).Scan(&n)
	return n, err
}

func (s *PGStore) ListCatalogTracks() ([]CatalogTrack, error) {
	// Статус трека: 'ready', когда готовы и аудио-фичи, и вектор активной
	// модели; иначе статус аудио-фичей (отсутствие строки = pending).
	embJoin := ""
	statusExpr := `COALESCE(f.status,'pending')`
	if _, _, emb := s.ActiveModel(); emb != "" {
		embJoin = `LEFT JOIN ` + emb + ` e ON e.track_id = t.id`
		statusExpr = `CASE WHEN COALESCE(f.status,'ready') = 'ready' AND e.status = 'ready'
			THEN 'ready' ELSE COALESCE(f.status,'pending') END`
	}
	rows, err := s.DB.Query(`
SELECT t.id, t.path, COALESCE(t.title,''), COALESCE(t.artist,''), COALESCE(t.album,''),
       COALESCE(t.duration,0), COALESCE(t.artwork_path,''), COALESCE(f.cluster_id,-1),
       ` + statusExpr + `
FROM tracks t
LEFT JOIN track_audio_features f ON f.track_id = t.id
` + embJoin + `
WHERE t.is_active AND t.is_duplicate_of IS NULL
ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogTrack
	for rows.Next() {
		var tr CatalogTrack
		if err := rows.Scan(&tr.ID, &tr.Path, &tr.Title, &tr.Artist, &tr.Album,
			&tr.Duration, &tr.Artwork, &tr.Cluster, &tr.Status); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

func (s *PGStore) TrackPath(id int64) (string, error) {
	var path string
	err := s.DB.QueryRow(
		`SELECT path FROM tracks WHERE id = $1 AND is_active`, id).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return path, err
}

// TrackArtworkPath реализует Backend (см. комментарий в backend.go).
func (s *PGStore) TrackArtworkPath(id int64) (string, bool) {
	var path string
	err := s.DB.QueryRow(
		`SELECT COALESCE(artwork_path, '') FROM tracks WHERE id = $1 AND is_active`, id,
	).Scan(&path)
	if err != nil || path == "" {
		return "", false
	}
	return path, true
}
