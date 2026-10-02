package db

import (
	"database/sql"
	"fmt"
	"time"
)

func (s *PGStore) CreateRadioShare(userID int64, token, name string) (RadioShare, error) {
	now := time.Now().UTC()
	_, err := s.DB.Exec(`
INSERT INTO radio_shares(user_id, token, name, created_at, listen_count)
VALUES ($1,$2,$3,$4,0)`, userID, token, name, now)
	if err != nil {
		return RadioShare{}, err
	}
	return RadioShare{Token: token, Name: name, CreatedAt: now.Format(time.RFC3339Nano), Active: true}, nil
}

func (s *PGStore) ListRadioShares(userID int64, includeRevoked bool) ([]RadioShare, error) {
	q := `SELECT token, name, created_at, revoked_at, last_listen_at, listen_count
FROM radio_shares WHERE user_id = $1`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.DB.Query(q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RadioShare
	for rows.Next() {
		var sh RadioShare
		var revoked, last sql.NullString
		if err := rows.Scan(&sh.Token, &sh.Name, &sh.CreatedAt, &revoked, &last, &sh.ListenCount); err != nil {
			return nil, err
		}
		if revoked.Valid {
			sh.RevokedAt = &revoked.String
		}
		if last.Valid {
			sh.LastListenAt = &last.String
		}
		sh.Active = !revoked.Valid
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *PGStore) GetActiveRadioShare(token string) (RadioShare, bool, error) {
	var sh RadioShare
	var revoked, last sql.NullString
	err := s.DB.QueryRow(`
SELECT token, user_id, name, created_at, revoked_at, last_listen_at, listen_count
FROM radio_shares WHERE token = $1`, token).Scan(
		&sh.Token, &sh.UserID, &sh.Name, &sh.CreatedAt, &revoked, &last, &sh.ListenCount)
	if err == sql.ErrNoRows {
		return RadioShare{}, false, nil
	}
	if err != nil {
		return RadioShare{}, false, err
	}
	if revoked.Valid {
		sh.RevokedAt = &revoked.String
		return sh, false, nil
	}
	if last.Valid {
		sh.LastListenAt = &last.String
	}
	sh.Active = true
	return sh, true, nil
}

// RevokeRadioShare скопирован по пользователю: чужой токен = not found.
func (s *PGStore) RevokeRadioShare(userID int64, token string) error {
	res, err := s.DB.Exec(
		`UPDATE radio_shares SET revoked_at = now()
WHERE token = $1 AND user_id = $2 AND revoked_at IS NULL`, token, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("not found")
	}
	return nil
}

func (s *PGStore) TouchRadioShareListen(token string) error {
	_, err := s.DB.Exec(`
UPDATE radio_shares SET listen_count = listen_count + 1, last_listen_at = now()
WHERE token = $1 AND revoked_at IS NULL`, token)
	return err
}
