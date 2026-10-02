// Middleware слоя /rest/* — производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/middlewares.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// minClient* — минимальная версия клиента: ниже 1.8.0 нет token+salt
// в стабильном виде и ID3-методов; отдаём code 20.
const minClientMajor, minClientMinor = 1, 8

// postFormToQueryParams мержит POST-тела (application/x-www-form-urlencoded)
// в query-параметры до хендлеров: клиенты шлют параметры и так, и так.
// Лимит тела — 10MB.
func (rt *Router) postFormToQueryParams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		if err := r.ParseForm(); err != nil {
			rt.sendError(w, r, NewError(ErrorGeneric, err.Error()))
			return
		}
		var parts []string
		for key, values := range r.Form {
			for _, v := range values {
				parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(v))
			}
		}
		r.URL.RawQuery = strings.Join(parts, "&")
		next.ServeHTTP(w, r)
	})
}

// checkRequiredParameters — обязательные u, v, c (code 10) и мягкая
// проверка версии клиента (>= 1.8, code 20; непарсящаяся версия проходит).
func (rt *Router) checkRequiredParameters(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	for _, p := range []string{"u", "v", "c"} {
		if q.Get(p) == "" {
			rt.sendError(w, r, NewError(ErrorMissingParameter, "required parameter "+p+" is missing"))
			return false
		}
	}
	if v := q.Get("v"); v != "" && clientTooOld(v) {
		rt.sendError(w, r, NewError(ErrorClientTooOld, ""))
		return false
	}
	return true
}

// clientTooOld — v < 1.8.0; «1.8», «1.8.0», «1.16.1» — ок.
func clientTooOld(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false // мягкая проверка: непарсящееся — пропускаем
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return major < minClientMajor || (major == minClientMajor && minor < minClientMinor)
}

// authenticate — аутентификация Subsonic (F3.1): u + (t+s | p | p=enc:).
// Subsonic-пароль отдельный от основного argon2id; md5(clear) хранится в
// users.subsonic_md5, clear — AES-GCM в users.subsonic_password_enc.
// Все отказы — единый code 40 (не раскрываем причину, как Navidrome);
// rate-limit ключа IP|username — тоже 40.
func (rt *Router) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if rt.Disabled {
		rt.sendError(w, r, NewError(ErrorAuthorizationFail,
			"Subsonic authentication is disabled by the server configuration (MUSIC_HIVE_SUBSONIC_AUTH=0)"))
		return false
	}
	q := r.URL.Query()
	username := q.Get("u")
	if rt.Limiter != nil && rt.Limiter.BlockedKey(r, username) {
		rt.sendError(w, r, NewError(ErrorAuthenticationFail, ""))
		return false
	}
	creds, found, err := rt.Users.AuthSubsonicCredentials(username)
	if err != nil {
		log.Printf("subsonic: auth lookup %q: %v", username, err)
		rt.deny(r, username)
		rt.sendError(w, r, NewError(ErrorAuthenticationFail, ""))
		return false
	}
	if !found || creds.Disabled || !validateCredentials(rt.Cipher, creds, q.Get("t"), q.Get("s"), q.Get("p")) {
		rt.deny(r, username)
		rt.sendError(w, r, NewError(ErrorAuthenticationFail, ""))
		return false
	}
	*r = *r.WithContext(auth.WithUser(r.Context(), creds.User))
	return true
}

func (rt *Router) deny(r *http.Request, username string) {
	if rt.Limiter != nil {
		rt.Limiter.DenyKey(r, username)
	}
}

// validateCredentials — три схемы протокола (приоритет t= > p=, как у
// Navidrome):
//
//	t + s: t == md5hex(clear + s) — clear расшифровывается из БД;
//	p:     p == clear (сравнение md5hex(p) с users.subsonic_md5);
//	p=enc:<hex>: то же после hex-декодирования.
func validateCredentials(cipher *auth.SubsonicCipher, creds auth.SubsonicCredentials, token, salt, pass string) bool {
	switch {
	case token != "":
		clear, err := cipher.Open(creds.PasswordEnc)
		if err != nil {
			return false
		}
		want := auth.MD5Hex(clear + salt)
		return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(token))) == 1
	case pass != "":
		if rest, ok := strings.CutPrefix(pass, "enc:"); ok {
			dec, err := hex.DecodeString(rest)
			if err != nil {
				return false
			}
			pass = string(dec)
		}
		want := auth.MD5Hex(pass)
		return subtle.ConstantTimeCompare([]byte(want), []byte(creds.SubsonicMD5)) == 1
	}
	return false
}
