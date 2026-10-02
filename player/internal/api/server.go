package api

import (
	"log"
	"net/http"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/app"
	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/media"
	"github.com/spa-skyson/music-hive/player/internal/playback"
	"github.com/spa-skyson/music-hive/player/internal/queue"
	"github.com/spa-skyson/music-hive/player/internal/subsonic"
)

const (
	Version    = "1.0.0"
	APIVersion = "v1"
)

type Server struct {
	Cfg     config.Config
	Store   db.Backend
	Idx     index.Searcher
	Builder *queue.Builder
	Play    *playback.Engine
	App     *app.Service
	Static  http.FileSystem
	HTTP    *http.Client
	Auth    *auth.Gate

	shareListeners int32
	loginLimiter   *auth.LoginLimiter
	latency        *latencyRecorder
	backgroundIO   chan func()

	Media *media.Service

	// subsonic — /rest/*-маршруты (F3.1).
	subsonic *subsonic.Router
}

type routeDescriptor struct {
	method  string
	path    string
	handler func(*Server, http.ResponseWriter, *http.Request)
}

var apiRoutes = []routeDescriptor{
	{"GET", "/api/health", (*Server).handleHealth},
	{"GET", "/api/openapi.json", (*Server).handleOpenAPI},
	{"GET", "/api/auth/me", (*Server).handleAuthMe},
	{"POST", "/api/auth/login", (*Server).handleAuthLogin},
	{"POST", "/api/auth/logout", (*Server).handleAuthLogout},
	{"PUT", "/api/auth/subsonic-password", (*Server).handleAuthSubsonicPassword},
	{"POST", "/api/auth/tokens", (*Server).handleAuthTokens},
	{"GET", "/api/auth/tokens", (*Server).handleAuthTokensList},
	{"DELETE", "/api/auth/tokens/{id}", (*Server).handleAuthTokensRevoke},
	{"GET", "/api/admin/users", (*Server).handleAdminUsersList},
	{"POST", "/api/admin/users", (*Server).handleAdminUsersCreate},
	{"PATCH", "/api/admin/users/{id}", (*Server).handleAdminUsersPatch},
	{"GET", "/api/admin/models", (*Server).handleAdminModelsStatus},
	{"POST", "/api/admin/models/activate", (*Server).handleAdminModelsActivate},
	{"GET", "/api/status", (*Server).handleStatus},
	{"GET", "/api/profile", (*Server).handleProfile},
	{"GET", "/api/library", (*Server).handleLibrary},
	{"GET", "/api/artists", (*Server).handleArtists},
	{"GET", "/api/albums", (*Server).handleAlbums},
	{"GET", "/api/tracks/{id}", (*Server).handleTrack},
	{"GET", "/api/stream/{id}", (*Server).handleStream},
	{"GET", "/api/artwork/{id}", (*Server).handleArtwork},
	{"GET", "/api/similar/{id}", (*Server).handleSimilar},
	{"POST", "/api/reload", (*Server).handleReload},
	{"POST", "/api/session/start", (*Server).handleSessionStart},
	{"POST", "/api/session/jump", (*Server).handleSessionJump},
	{"POST", "/api/radio/start", (*Server).handleRadioStart},
	{"POST", "/api/share/radio", (*Server).handleShareRadioCreate},
	{"GET", "/api/share/radio", (*Server).handleShareRadioList},
	{"DELETE", "/api/share/radio/{token}", (*Server).handleShareRadioRevoke},
	{"GET", "/listen/{token}", (*Server).handleListenShare},
	{"POST", "/api/play", (*Server).handlePlay},
	{"POST", "/api/session/shuffle", (*Server).handleSessionShuffle},
	{"POST", "/api/events", (*Server).handleEvents},
	{"GET", "/api/now", (*Server).handleNow},
	{"GET", "/api/tracks/{id}/lyrics", (*Server).handleTrackLyrics},
	{"GET", "/api/mixes", (*Server).handleMixes},
	{"POST", "/api/mixes/{kind}/play", (*Server).handleMixPlay},
	{"GET", "/api/later", (*Server).handleLaterList},
	{"POST", "/api/later", (*Server).handleLaterAdd},
	{"DELETE", "/api/later", (*Server).handleLaterRemove},
	{"GET", "/api/favorites", (*Server).handleFavoritesList},
	{"POST", "/api/favorites", (*Server).handleFavoritesAdd},
	{"DELETE", "/api/favorites", (*Server).handleFavoritesRemove},
	{"POST", "/api/favorites/toggle", (*Server).handleFavoritesToggle},
	{"GET", "/api/favorites/status", (*Server).handleFavoritesStatus},
	{"GET", "/api/similar/artists", (*Server).handleSimilarArtists},
	{"GET", "/api/similar/albums", (*Server).handleSimilarAlbums},
	{"GET", "/api/recommend/favorites", (*Server).handleRecommendFavorites},
	{"GET", "/api/recommend/seed", (*Server).handleRecommendSeed},
	{"GET", "/api/discover/albums", (*Server).handleDiscoverAlbums},
	{"GET", "/api/discover/resurfaced", (*Server).handleDiscoverResurfaced},
	{"POST", "/api/library/upload", (*Server).handleLibraryUpload},
	{"POST", "/api/library/rescan", (*Server).handleLibraryRescan},
	{"POST", "/api/jobs/{kind}", (*Server).handleEnqueueJob},
	{"GET", "/api/jobs/{id}", (*Server).handleGetJob},
	{"GET", "/api/jobs", (*Server).handleListJobs},
	{"GET", "/api/metrics/weekly", (*Server).handleWeeklyMetrics},
	{"GET", "/api/metrics/recommendations", (*Server).handleRecommendationMetrics},
	{"GET", "/manifest.webmanifest", (*Server).handleManifest},
}

func New(cfg config.Config, store db.Backend, idx index.Searcher, staticFS http.FileSystem) *Server {
	gate := auth.New(auth.Config{
		Disabled:     cfg.AuthDisabled,
		SecureCookie: cfg.SecureCookie,
	})
	builder := queue.NewBuilder(idx, cfg)
	s := &Server{
		Cfg: cfg, Store: store, Idx: idx,
		Builder:      builder,
		Play:         playback.New(cfg, store, idx, builder),
		Static:       staticFS,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		Auth:         gate,
		loginLimiter: auth.NewLoginLimiter(5, time.Minute),
		latency:      newLatencyRecorder(),
		backgroundIO: make(chan func(), 256),
	}
	// PG-режим (F2.1): gate получает серверное хранилище пользователей/
	// сессий/токенов.
	if pg, ok := store.(auth.Store); ok {
		gate.Users = pg
	}
	// Subsonic /rest/* (F3.1): шифратор subsonic-пароля (ключ —
	// MUSIC_HIVE_SUBSONIC_SECRET либо HKDF от сессионного секрета).
	cipherIKM := []byte(cfg.SessionSecret)
	if len(cipherIKM) < 16 {
		cipherIKM = auth.DerivedSecret(cfg.Password)
	}
	cipher, err := auth.NewSubsonicCipher(cfg.SubsonicSecret, cipherIKM)
	if err != nil {
		cipher = nil // t=-схема недоступна; p= работает по md5
		log.Printf("warning: subsonic cipher unavailable (token auth disabled): %v", err)
	}
	if ss, ok := store.(subsonic.Store); ok && gate.Users != nil {
		srt := &subsonic.Router{
			Users:         ss,
			Cipher:        cipher,
			Limiter:       s.loginLimiter,
			Disabled:      cfg.SubsonicAuthDisabled,
			ServerVersion: Version,
			PublicBaseURL: cfg.PublicBaseURL,
		}
		if cat, ok := store.(subsonic.Catalog); ok { // каталог (F3.2, #24)
			srt.Catalog = cat
		}
		if lib, ok := store.(subsonic.Library); ok { // star/scrobble (F3.3, #25)
			srt.Library = lib
		}
		// F3.4 (#26): плейлисты, play-queue, закладки, радио, пользователи,
		// скан; похожие треки — адаптер к recommend (subsonic не может
		// импортировать index/recommend — цикл через db).
		if pls, ok := store.(subsonic.PlaylistStore); ok {
			srt.Playlists = pls
		}
		if qs, ok := store.(subsonic.QueueStore); ok {
			srt.Queues = qs
		}
		if bm, ok := store.(subsonic.BookmarkStore); ok {
			srt.Bookmarks = bm
		}
		if rd, ok := store.(subsonic.RadioStore); ok {
			srt.Radio = rd
		}
		if acc, ok := store.(subsonic.UserAccounts); ok {
			srt.Accounts = acc
		}
		if sc, ok := store.(subsonic.ScanStore); ok {
			srt.Scan = sc
		}
		srt.Similar = s.subsonicSimilar
		s.subsonic = srt
	}
	s.App = app.New(cfg, store, idx, s.Play, s.HTTP)
	s.Media = media.New(cfg, idx, store)
	if s.subsonic != nil {
		s.subsonic.Media = s.Media // стрим/обложки — общий media-слос с /api/stream
	}
	s.Play.Enqueue = s.enqueueBackgroundIO
	s.Play.Flush = s.flushBackgroundIO
	s.Play.Observe = func(op string, d time.Duration) { s.latency.Observe(op, d) }
	s.Play.Warm = s.Media.Warm
	go s.runBackgroundIO()
	return s
}

func (s *Server) runBackgroundIO() {
	for work := range s.backgroundIO {
		work()
	}
}

func (s *Server) enqueueBackgroundIO(work func()) {
	if work == nil {
		return
	}
	s.backgroundIO <- work
}

func (s *Server) flushBackgroundIO() {
	done := make(chan struct{})
	s.enqueueBackgroundIO(func() { close(done) })
	<-done
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range apiRoutes {
		mux.HandleFunc(route.method+" "+route.path, func(w http.ResponseWriter, r *http.Request) {
			route.handler(s, w, r)
		})
	}
	// Subsonic /rest/* (F3.1): отдельный маршрутизатор со своей auth.
	if s.subsonic != nil {
		mux.Handle("/rest/", http.StripPrefix("/rest", s.subsonic.Handler()))
	}
	mux.Handle("/", s.staticHandler())
	var h http.Handler = mux
	// origin-guard внутри auth.Middleware: видит способ аутентификации
	// (cookie/Bearer/owner) в context и применяется только к cookie-пути
	// (issue #46).
	h = s.withOriginGuard(h)
	if s.Auth != nil {
		h = s.Auth.Middleware(h)
	}
	// withSecurityHeaders — внешний слой: CSP/nosniff/Referrer-Policy на все
	// ответы, включая статику и preflight (issue #46).
	return withSecurityHeaders(withCORS(withAPICacheControl(h), s.Cfg.CORSOrigins))
}
