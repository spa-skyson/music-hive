package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	DBPath               string
	DatabaseURL          string // MUSIC_HIVE_DATABASE_URL: PostgreSQL (F1.3), обязателен
	Library              string
	Addr                 string
	ExploreRatio         float64
	DiscoverExploreRatio float64
	ProfileReadyAt       int
	ProfileFormingAt     int
	NewTrackDays         int
	QueueSize            int
	TasteAlpha           float64
	NewBoostBeta         float64
	NewBoostTauDays      float64
	NewBoostGamma        float64
	WorkerURL            string
	WorkerAutostart      bool
	NewAlbumDays         int
	// Password — MUSIC_HIVE_PASSWORD: bootstrap владельца PG (решение
	// F2.1, auth.BootstrapOwner). Для PG-аутентификации не требуется —
	// только чтобы задать/вернуть пароль владельца через env.
	Password          string
	SessionSecret     string
	AuthDisabled      bool
	SecureCookie      bool
	PublicBaseURL     string
	FFmpegPath        string
	ShareBitrate      string
	ShareMaxListeners int
	MobileBitrate     string // e.g. 160k — Android / LTE stream profile
	MobileFormat      string // aac | mp3
	CORSOrigins       []string
	CandidatePoolAt   int // N at which queue uses shortlist (default 8000)
	// ReloadProbeSec — период probe-цикла перезагрузки индекса (GitLab #47,
	// сек, default 60; 0/отрицательное — выключено).
	ReloadProbeSec       int
	SubsonicSecret       string
	SubsonicAuthDisabled bool
}

func Load() Config {
	if legacy := legacyMusikEnvVars(os.Environ()); len(legacy) > 0 {
		log.Printf("warning: обнаружены переменные старого формата musik; переименуйте (MUSIK_* → MUSIC_HIVE_*): %s", strings.Join(legacy, ", "))
	}
	root := findRoot()
	db := env("MUSIC_HIVE_DB_PATH", filepath.Join(root, "data", "db", "music-hive.db"))
	return Config{
		DBPath:               db,
		DatabaseURL:          env("MUSIC_HIVE_DATABASE_URL", ""),
		Library:              env("MUSIC_HIVE_LIBRARY", filepath.Join(root, "data", "music")),
		Addr:                 env("MUSIC_HIVE_PLAYER_ADDR", ":8787"),
		ExploreRatio:         envFloat("MUSIC_HIVE_EXPLORE_RATIO", 0.15),
		DiscoverExploreRatio: envFloat("MUSIC_HIVE_DISCOVER_EXPLORE", 0.35),
		ProfileReadyAt:       envInt("MUSIC_HIVE_PROFILE_READY_AT", 8),
		ProfileFormingAt:     envInt("MUSIC_HIVE_PROFILE_FORMING_AT", 3),
		NewTrackDays:         envInt("MUSIC_HIVE_NEW_TRACK_DAYS", 14),
		QueueSize:            envInt("MUSIC_HIVE_QUEUE_SIZE", 6),
		TasteAlpha:           envFloat("MUSIC_HIVE_TASTE_ALPHA", 0.22),
		NewBoostBeta:         envFloat("MUSIC_HIVE_NEW_BOOST_BETA", 0.25),
		NewBoostTauDays:      envFloat("MUSIC_HIVE_NEW_BOOST_TAU", 14),
		NewBoostGamma:        envFloat("MUSIC_HIVE_NEW_BOOST_GAMMA", 5),
		WorkerURL:            env("MUSIC_HIVE_WORKER_URL", "http://127.0.0.1:8790"),
		WorkerAutostart:      envBool("MUSIC_HIVE_WORKER_AUTOSTART", true),
		NewAlbumDays:         envInt("MUSIC_HIVE_NEW_ALBUM_DAYS", 14),
		Password:             env("MUSIC_HIVE_PASSWORD", ""),
		SessionSecret:        env("MUSIC_HIVE_SESSION_SECRET", ""),
		AuthDisabled:         envBool("MUSIC_HIVE_AUTH_DISABLED", false),
		SecureCookie:         envBool("MUSIC_HIVE_SECURE_COOKIE", false),
		PublicBaseURL:        stringsTrimRightSlash(env("MUSIC_HIVE_PUBLIC_BASE_URL", "")),
		FFmpegPath:           env("MUSIC_HIVE_FFMPEG", "ffmpeg"),
		ShareBitrate:         env("MUSIC_HIVE_SHARE_BITRATE", "192k"),
		ShareMaxListeners:    envInt("MUSIC_HIVE_SHARE_MAX_LISTENERS", 4),
		MobileBitrate:        env("MUSIC_HIVE_MOBILE_BITRATE", "160k"),
		MobileFormat:         stringsToLower(env("MUSIC_HIVE_MOBILE_FORMAT", "aac")),
		CORSOrigins:          splitCSV(env("MUSIC_HIVE_CORS_ORIGINS", "")),
		CandidatePoolAt:      envInt("MUSIC_HIVE_CANDIDATE_POOL_AT", 8000),
		ReloadProbeSec:       envInt("MUSIC_HIVE_RELOAD_PROBE_SEC", 60),
		SubsonicSecret:       env("MUSIC_HIVE_SUBSONIC_SECRET", ""),
		// MUSIC_HIVE_SUBSONIC_AUTH=0 — выключить subsonic-аутентификацию
		// (/rest/*-запросы с авторизацией → ошибка 50 с пояснением).
		SubsonicAuthDisabled: !envBool("MUSIC_HIVE_SUBSONIC_AUTH", true),
	}
}

func stringsToLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := s[start:i]
			for len(part) > 0 && (part[0] == ' ' || part[0] == '\t') {
				part = part[1:]
			}
			for len(part) > 0 && (part[len(part)-1] == ' ' || part[len(part)-1] == '\t') {
				part = part[:len(part)-1]
			}
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func stringsTrimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// AuthEnabled — включена ли аутентификация: PG-режим требует её всегда
// (владелец гарантирован bootstrap'ом), MUSIC_HIVE_AUTH_DISABLED=1 выключает.
func (c Config) AuthEnabled() bool {
	return !c.AuthDisabled
}

// Validate — fail-fast проверка небезопасных комбинаций конфига на старте.
//
// Wildcard "*" в MUSIC_HIVE_CORS_ORIGINS запрещён (issue #46): API всегда
// отвечает с Access-Control-Allow-Credentials: true (cookie-сессии F2.1),
// а сочетание "Access-Control-Allow-Origin" для любого origin с credentials
// запрещено спецификацией Fetch (credentialed-ответы на "*" не читаются) и
// на практике раздаёт cookie-доступ любому сайту, обнуляя CSRF-защиту.
// Перечисляйте доверенные origin'ы явно: MUSIC_HIVE_CORS_ORIGINS=https://hive.example
func (c Config) Validate() error {
	for _, o := range c.CORSOrigins {
		if o == "*" {
			return fmt.Errorf(
				"MUSIC_HIVE_CORS_ORIGINS: wildcard \"*\" запрещён: API шлёт Access-Control-Allow-Credentials: true (cookie-сессии), а \"*\"+credentials нельзя по спецификации CORS; перечислите доверенные origin'ы явно")
		}
	}
	return nil
}

func findRoot() string {
	if r := os.Getenv("MUSIC_HIVE_ROOT"); r != "" {
		return r
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := cwd
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "data", "db", "music-hive.db")
		if _, err := os.Stat(candidate); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd
}

// legacyMusikEnvVars returns names of env vars in the old musik format:
// prefixed MUSIK_ but not the new MUSIC_HIVE_.
func legacyMusikEnvVars(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "MUSIK_") && !strings.HasPrefix(name, "MUSIC_HIVE_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	case "0", "false", "FALSE", "no", "off":
		return false
	default:
		return def
	}
}
