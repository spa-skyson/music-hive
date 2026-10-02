package api

import (
	"encoding/json"
	"errors"
	"github.com/spa-skyson/music-hive/player/internal/auth"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func withCORS(next http.Handler, allowed []string) http.Handler {
	allowSet := map[string]bool{}
	for _, o := range allowed {
		if o == "*" {
			// Issue #46: wildcard + Access-Control-Allow-Credentials:true
			// запрещён (config.Validate фейлит старт раньше); здесь —
			// страховка на случай сборки Config в коде мимо Load: "*"
			// игнорируется, деградация до same-origin only.
			log.Printf("security: CORS wildcard \"*\" игнорируется (запрещён вместе с credentials, issue #46)")
			continue
		}
		allowSet[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			ok := false
			if len(allowSet) == 0 {
				// same-origin only: reflect if Origin host matches request Host
				if u, err := parseOriginHost(origin); err == nil && u == r.Host {
					ok = true
				}
			} else if allowSet[origin] || allowSet["*"] {
				ok = true
			}
			if ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseOriginHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", errors.New("bad origin")
	}
	return u.Host, nil
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "code": errCode})
}

func playHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	switch err.Error() {
	case "нет доступных треков", "ничего не найдено", "track not found", "track not in playlist":
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) staticHandler() http.Handler {
	if s.Static == nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			// Vite-бандлы: имя файла содержит хеш содержимого, безопасно
			// кэшировать навечно (Q13, issue #43).
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case filepath.Ext(r.URL.Path) == ".js" || filepath.Ext(r.URL.Path) == ".css":
			w.Header().Set("Cache-Control", "public, max-age=3600")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.URL.Path == "/" || r.URL.Path == "" {
			f, err := s.Static.Open("index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			stat, err := f.Stat()
			if err != nil {
				http.NotFound(w, r)
				return
			}
			rs, ok := f.(io.ReadSeeker)
			if !ok {
				r.URL.Path = "/index.html"
				fileServer.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", modTime(stat), rs)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func withAPICacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			if len(r.URL.Path) >= 13 && r.URL.Path[:13] == "/api/artwork/" {
				w.Header().Set("Cache-Control", "private, max-age=3600")
			} else {
				w.Header().Set("Cache-Control", "no-store")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func modTime(info fs.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	return info.ModTime()
}

// dbLabel — человекочитаемый идентификатор хранилища для /api/status.
// requestUserID — пользователь запроса из context (auth-мидлварь, F2.2);
// на защищённых путях мидлварь гарантирует пользователя.
func requestUserID(r *http.Request) int64 {
	if u, ok := auth.FromContext(r.Context()); ok {
		return u.ID
	}
	return 0
}

func (s *Server) dbLabel() string {
	if s.Cfg.DatabaseURL != "" {
		return "postgres"
	}
	return s.Cfg.DBPath
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"ok": true, "version": Version, "api_version": APIVersion,
		"auth": s.Auth != nil && s.Auth.Enabled(),
	}
	if s.Auth == nil || !s.Auth.Enabled() || s.Auth.Authenticated(r) {
		out["tracks"] = s.Idx.Size()
		out["dim"] = s.Idx.Dim()
	}
	writeJSON(w, out)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	uid := requestUserID(r)
	mat := s.Play.MaturityOf(uid)
	explore := s.Play.ExploreOf(uid)
	sessionCount := s.Play.SessionCount()
	out := map[string]any{
		"tracks": s.Idx.Size(), "dim": s.Idx.Dim(),
		"taste_ready":      s.Play.TasteOf(uid).Ready(),
		"maturity":         mat,
		"sessions":         sessionCount,
		"explore":          explore,
		"db":               s.dbLabel(),
		"worker_url":       s.Cfg.WorkerURL,
		"worker_autostart": s.Cfg.WorkerAutostart,
	}
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		if sess := s.Play.Get(uid, sid); sess != nil {
			sess.Lock()
			out["session_id"] = sess.ID
			out["mode"] = sess.Mode
			out["current"] = sess.Current
			out["queue_len"] = len(sess.Queue)
			sess.Unlock()
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	_, _ = w.Write([]byte(`{
  "name": "Music Hive",
  "short_name": "Hive",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#141210",
  "theme_color": "#c45c26",
  "description": "Local smart music player"
}`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func contentType(path string) string {
	switch filepath.Ext(path) {
	case ".mp3":
		return "audio/mpeg"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}
