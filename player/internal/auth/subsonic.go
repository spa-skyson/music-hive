package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// SubsonicCredentials — подокно учётных данных для аутентификации Subsonic
// /rest/* (F3.1, GitLab #23). Основной argon2id-пароль здесь не участвует:
// token+salt требует clear-пароль, поэтому он хранится обратимо (AES-GCM),
// а md5(clear) — для сравнения p= без расшифровки.
type SubsonicCredentials struct {
	User         User
	Disabled     bool
	SubsonicMD5  string // md5hex(clear-пароля); "" — subsonic-пароль не задан
	PasswordEnc  string // base64(nonce+AES-GCM(clear)); "" — не задан
	PasswordHash string // argon2id основного пароля (для dummy-проверок)
}

// SubsonicCipher — AES-256-GCM для обратимого хранения subsonic-пароля.
// Ключ: SHA-256(MUSIC_HIVE_SUBSONIC_SECRET); при отсутствии секрета —
// HKDF-SHA256 от сессионного секрета с домен-сепаратором "subsonic-enc"
// (ротация SESSION_SECRET/секрета инвалидирует сохранённые пароли).
type SubsonicCipher struct {
	aead cipher.AEAD
}

// NewSubsonicCipher собирает шифратор. ikm — сессионный секрет
// (env MUSIC_HIVE_SESSION_SECRET либо дериват от пароля, как Gate).
func NewSubsonicCipher(subsonicSecret string, sessionIKM []byte) (*SubsonicCipher, error) {
	key := make([]byte, 32)
	if subsonicSecret != "" {
		sum := sha256.Sum256([]byte(subsonicSecret))
		copy(key, sum[:])
	} else {
		h := hkdf.New(sha256.New, sessionIKM, nil, []byte("subsonic-enc"))
		if _, err := io.ReadFull(h, key); err != nil {
			return nil, fmt.Errorf("subsonic cipher: hkdf: %w", err)
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("subsonic cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("subsonic cipher: gcm: %w", err)
	}
	return &SubsonicCipher{aead: aead}, nil
}

// Seal шифрует clear-пароль → base64(nonce||ciphertext).
func (c *SubsonicCipher) Seal(clear string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("subsonic cipher: not initialized")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("subsonic cipher: nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, []byte(clear), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Open восстанавливает clear-пароль; ошибка = неверный блоб/ключ.
func (c *SubsonicCipher) Open(enc string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("subsonic cipher: not initialized")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("subsonic cipher: blob: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) <= ns {
		return "", fmt.Errorf("subsonic cipher: short blob")
	}
	clear, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("subsonic cipher: open: %w", err)
	}
	return string(clear), nil
}

// MD5Hex — hex(md5(s)). md5 здесь не про криптостойкость, а про протокол:
// Subsonic token+salt = md5hex(password+salt) считает клиент (F3.1).
func MD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
