package api

import (
	"net/http"

	"github.com/spa-skyson/music-hive/player/internal/taste"
)

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	uid := requestUserID(r)
	tp := s.Play.TasteOf(uid)
	pos, neg := tp.Counts()
	if dbPos, dbNeg, err := s.Store.ListenSignalCounts(uid); err == nil {
		// keep RAM counters at least as high as DB (reload may lag)
		if dbPos > pos {
			pos = dbPos
		}
		if dbNeg > neg {
			neg = dbNeg
		}
		tp.SetCounts(pos, neg)
	}
	mat := tp.Maturity(s.Cfg.ProfileFormingAt, s.Cfg.ProfileReadyAt)
	explore := tp.EffectiveExplore(s.Cfg.ExploreRatio, s.Cfg.DiscoverExploreRatio,
		s.Cfg.ProfileFormingAt, s.Cfg.ProfileReadyAt)
	artists, _ := s.Store.TopArtists(uid, 5)
	clusters, _ := s.Store.TopClusters(uid, 5)
	confidence := float64(pos) / float64(s.Cfg.ProfileReadyAt)
	if confidence > 1 {
		confidence = 1
	}
	writeJSON(w, map[string]any{
		"ready":            mat == taste.StatusReady,
		"maturity":         mat,
		"n_positive":       pos,
		"n_negative":       neg,
		"ready_at":         s.Cfg.ProfileReadyAt,
		"forming_at":       s.Cfg.ProfileFormingAt,
		"confidence":       confidence,
		"explore_ratio":    explore,
		"source":           tp.SourceName(),
		"taste_vector":     tp.Ready(),
		"top_artists":      artists,
		"top_clusters":     clusters,
		"online_authority": "go_ema",
		"online_context":   "global",
		"offline_context":  "offline_report",
		"offline_note":     "Python writes only offline_report; Go owns global EMA",
	})
}
