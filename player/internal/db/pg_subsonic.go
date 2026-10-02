package db

import (
	"database/sql"
	"fmt"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

// PGStore: подокно subsonic-аутентификации (F3.1, GitLab #23). Схема —
// users.subsonic_md5 (00001) + users.subsonic_password_enc (00003).

var _ subsonic.Store = (*PGStore)(nil)

// AuthSubsonicCredentials — учётные данные для /rest/*: md5(clear) для p=,
// AES-GCM clear для token+salt; disabled — фильтруется здесь же, вызывающий
// отвечает единым code 40 (не раскрываем блокировку).
func (s *PGStore) AuthSubsonicCredentials(username string) (auth.SubsonicCredentials, bool, error) {
	var c auth.SubsonicCredentials
	err := s.DB.QueryRow(`
SELECT id, username, is_admin, is_owner, disabled, subsonic_md5, subsonic_password_enc
FROM users WHERE username = $1`, username).
		Scan(&c.User.ID, &c.User.Username, &c.User.IsAdmin, &c.User.IsOwner,
			&c.Disabled, &c.SubsonicMD5, &c.PasswordEnc)
	if err == sql.ErrNoRows {
		return auth.SubsonicCredentials{}, false, nil
	}
	if err != nil {
		return auth.SubsonicCredentials{}, false, fmt.Errorf("pg: subsonic credentials: %w", err)
	}
	return c, true, nil
}

// AuthSetSubsonicPassword — PUT /api/auth/subsonic-password: одновременно
// md5(clear) и шифрованный clear; либо оба заданы, либо оба пустые
// (пароль нельзя задать частично — схемы рассинхронятся).
func (s *PGStore) AuthSetSubsonicPassword(userID int64, md5hex, enc string) error {
	res, err := s.DB.Exec(`
UPDATE users SET subsonic_md5 = $2, subsonic_password_enc = $3
WHERE id = $1`, userID, md5hex, enc)
	if err != nil {
		return fmt.Errorf("pg: set subsonic password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pg: set subsonic password: user %d not found", userID)
	}
	return nil
}
