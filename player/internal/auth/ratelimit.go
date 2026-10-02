package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LoginLimiter caps password attempts per client IP.
type LoginLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func NewLoginLimiter(limit int, window time.Duration) *LoginLimiter {
	if limit < 1 {
		limit = 5
	}
	if window <= 0 {
		window = time.Minute
	}
	return &LoginLimiter{
		hits:   map[string][]time.Time{},
		limit:  limit,
		window: window,
	}
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// first hop only
		if i := len(xff); i > 0 {
			for j := 0; j < len(xff); j++ {
				if xff[j] == ',' {
					return trimSpace(xff[:j])
				}
			}
			return trimSpace(xff)
		}
	}
	return host
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// AllowKey — лимит по составному ключу IP|username (multi-user login,
// F2.1): брутфорс одного аккаунта блокируется, не задевая других
// пользователей того же IP.
func (l *LoginLimiter) AllowKey(r *http.Request, username string) bool {
	return l.allow(l.key(r, username))
}

func (l *LoginLimiter) key(r *http.Request, username string) string {
	return clientIP(r) + "|" + strings.ToLower(username)
}

// BlockedKey — заблокирован ли ключ, не засчитывая попытку. Для frequent-
// auth (Subsonic /rest/* шлёт u/t/s на каждый запрос, F3.1): гейт по
// счётчику, а засчитываются только неудачи (DenyKey).
func (l *LoginLimiter) BlockedKey(r *http.Request, username string) bool {
	if l == nil {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-l.window)
	k := l.key(r, username)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(k, cutoff)
	return len(l.hits[k]) >= l.limit
}

// DenyKey — зафиксировать неудачную попытку ключа IP|username.
func (l *LoginLimiter) DenyKey(r *http.Request, username string) {
	if l == nil {
		return
	}
	now := time.Now()
	cutoff := now.Add(-l.window)
	k := l.key(r, username)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(k, cutoff)
	l.hits[k] = append(l.hits[k], now)
}

// prune удаляет записи окна младше cutoff (вызывается под l.mu).
func (l *LoginLimiter) prune(key string, cutoff time.Time) {
	arr := l.hits[key]
	kept := arr[:0]
	for _, t := range arr {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = kept
	}
}

func (l *LoginLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(key, now.Add(-l.window))
	if len(l.hits[key]) >= l.limit {
		return false
	}
	l.hits[key] = append(l.hits[key], now)
	return true
}
