package db

import (
	"database/sql"
	"time"
)

func (s *PGStore) EnqueueJob(kind, payloadJSON string) (int64, error) {
	var id int64
	err := s.DB.QueryRow(`
INSERT INTO jobs(kind, status, payload_json, created_at, updated_at)
VALUES ($1,'pending',$2,now(),now()) RETURNING id`, kind, nullStr(payloadJSON)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// FindActiveJob возвращает последнюю джобу kind в статусе pending/running
// (guard от повторной постановки, #56) или nil, если активных нет.
// Статусы двигает воркер: pending → running (claim_next, lease+heartbeat),
// далее done/failed; зомби-running возвращает reaper.
func (s *PGStore) FindActiveJob(kind string) (*Job, error) {
	var j Job
	var payload, result, errStr sql.NullString
	err := s.DB.QueryRow(`
SELECT id, kind, status, payload_json::text, result_json::text, error, created_at, updated_at
FROM jobs WHERE kind = $1 AND status IN ('pending','running')
ORDER BY id DESC LIMIT 1`, kind).Scan(&j.ID, &j.Kind, &j.Status, &payload, &result, &errStr, &j.CreatedAt, &j.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Payload = payload.String
	j.Result = result.String
	j.Error = errStr.String
	return &j, nil
}

// ListDoneJobsAfter возвращает джобы, завершённые после timestamp
// (эксклюзивно). Используется плеером для auto-reload после worker'а.
func (s *PGStore) ListDoneJobsAfter(after string, limit int) ([]Job, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := s.DB.Query(`
SELECT id, kind, status, COALESCE(payload_json::text,''), COALESCE(result_json::text,''),
       COALESCE(error,''), created_at, updated_at
FROM jobs
WHERE status = 'done' AND updated_at > $1
ORDER BY updated_at ASC
LIMIT $2`, pgParseTimestamp(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *PGStore) ListJobs(status string, limit int) ([]Job, error) {
	if limit < 1 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.DB.Query(`
SELECT id, kind, status, COALESCE(payload_json::text,''), COALESCE(result_json::text,''),
       COALESCE(error,''), created_at, updated_at
FROM jobs WHERE status = $1 ORDER BY id DESC LIMIT $2`, status, limit)
	} else {
		rows, err = s.DB.Query(`
SELECT id, kind, status, COALESCE(payload_json::text,''), COALESCE(result_json::text,''),
       COALESCE(error,''), created_at, updated_at
FROM jobs ORDER BY id DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *PGStore) GetJob(id int64) (*Job, error) {
	var j Job
	var payload, result, errStr sql.NullString
	err := s.DB.QueryRow(`
SELECT id, kind, status, payload_json::text, result_json::text, error, created_at, updated_at
FROM jobs WHERE id = $1`, id).Scan(&j.ID, &j.Kind, &j.Status, &payload, &result, &errStr, &j.CreatedAt, &j.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Payload = payload.String
	j.Result = result.String
	j.Error = errStr.String
	return &j, nil
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.Status, &j.Payload, &j.Result, &j.Error, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// pgParseTimestamp принимает RFC3339-строку из WatchJobs (сканированную
// из timestamptz) и возвращает time.Time для сравнения.
func pgParseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// time.RFC3339 парсит и дробные секунды (RFC3339Nano — надмножество).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
