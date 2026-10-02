package db

import (
	"database/sql"
	"encoding/binary"
	"math"

	"github.com/pgvector/pgvector-go"
)

// Профиль вкуса: живая строка user_taste_profiles
// (user, context, model_key активной модели) вместо истории снимков.
// Векторы ходят little-endian float32-байтами; формат общий для
// верхних слоёв (playback/taste).

func (s *PGStore) SaveProfile(userID int64, context string, emb []byte) error {
	modelKey, _, _ := s.ActiveModel()
	if modelKey == "" || len(emb) == 0 || len(emb)%4 != 0 {
		return nil // модель не активирована или пустой вектор — хранить нечего
	}
	vec := pgvector.NewVector(float32FromBytes(emb))
	_, err := s.DB.Exec(`
INSERT INTO user_taste_profiles(user_id, context, model_key, vec, updated_at)
VALUES ($1,$2,$3,$4,now())
ON CONFLICT(user_id, context, model_key) DO UPDATE SET
  vec = excluded.vec, updated_at = now()`, userID, context, modelKey, vec)
	return err
}

// PruneProfiles: снимков больше нет — подчищаем профили неактивных моделей
// (актуальное пространство могло смениться, см. design §6.2).
func (s *PGStore) PruneProfiles(userID int64, context string, keep int) error {
	modelKey, _, _ := s.ActiveModel()
	if modelKey == "" {
		return nil
	}
	_, err := s.DB.Exec(`
DELETE FROM user_taste_profiles
WHERE user_id = $1 AND context = $2 AND model_key <> $3`, userID, context, modelKey)
	return err
}

func (s *PGStore) LatestProfile(userID int64, context string) ([]byte, error) {
	modelKey, _, _ := s.ActiveModel()
	if modelKey == "" {
		return nil, nil
	}
	var vec pgvector.Vector
	err := s.DB.QueryRow(`
SELECT vec FROM user_taste_profiles
WHERE user_id = $1 AND context = $2 AND model_key = $3
ORDER BY updated_at DESC LIMIT 1`, userID, context, modelKey).Scan(&vec)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return float32SliceToBytes(vec.Slice()), nil
}

// ListenSignalCounts считает позитивные/негативные сигналы для maturity.
func (s *PGStore) ListenSignalCounts(userID int64) (pos, neg int, err error) {
	err = s.DB.QueryRow(`
SELECT
  COALESCE(SUM(CASE
    WHEN action IN ('like','finish') THEN 1
    WHEN action = 'track_end' AND (reason IN ('completed','next') OR COALESCE(listened_sec,0) >= 0.8 * COALESCE(duration_sec,1)) THEN 1
    ELSE 0 END), 0),
  COALESCE(SUM(CASE
    WHEN action IN ('dislike','skip') THEN 1
    WHEN action = 'track_end' AND reason = 'skipped' AND COALESCE(listened_sec,0) < 0.3 * COALESCE(duration_sec,1) THEN 1
    ELSE 0 END), 0)
FROM listening_history WHERE user_id = $1`, userID).Scan(&pos, &neg)
	return
}

func (s *PGStore) TopArtists(userID int64, limit int) ([]ArtistCount, error) {
	if limit < 1 {
		limit = 5
	}
	rows, err := s.DB.Query(`
SELECT COALESCE(t.artist,'(unknown)'), COUNT(*) AS c
FROM listening_history h
JOIN tracks t ON t.id = h.track_id
WHERE h.user_id = $1
  AND h.action IN ('like','finish','track_end')
  AND (h.reason IS NULL OR h.reason != 'skipped')
GROUP BY 1
ORDER BY c DESC
LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArtistCount
	for rows.Next() {
		var a ArtistCount
		if err := rows.Scan(&a.Artist, &a.Count); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PGStore) TopClusters(userID int64, limit int) ([]ClusterCount, error) {
	if limit < 1 {
		limit = 5
	}
	rows, err := s.DB.Query(`
SELECT COALESCE(f.cluster_id, -1), COUNT(*) AS c
FROM listening_history h
JOIN track_audio_features f ON f.track_id = h.track_id
WHERE h.user_id = $1
  AND h.action IN ('like','finish','track_end')
  AND (h.reason IS NULL OR h.reason != 'skipped')
  AND f.cluster_id IS NOT NULL
GROUP BY 1
ORDER BY c DESC
LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClusterCount
	for rows.Next() {
		var c ClusterCount
		if err := rows.Scan(&c.ClusterID, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func float32FromBytes(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := range out {
		bits := binary.LittleEndian.Uint32(b[i*4 : (i+1)*4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

func float32SliceToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:(i+1)*4], math.Float32bits(f))
	}
	return b
}
