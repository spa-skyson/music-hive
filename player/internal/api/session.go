package api

import (
	"encoding/json"
	"net/http"

	"github.com/spa-skyson/music-hive/player/internal/playback"
)

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, bodySessionID string) *playback.Session {
	id := bodySessionID
	if id == "" {
		id = r.URL.Query().Get("session_id")
	}
	if id == "" {
		id = r.Header.Get("X-Session-Id")
	}
	// Чужая сессия не находится (404 — существование не раскрываем, F2.2).
	sess := s.Play.Get(requestUserID(r), id)
	if sess == nil {
		writeErr(w, 404, "session_not_found", "unknown or missing session_id — call /api/radio/start or /api/session/start")
		return nil
	}
	return sess
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SeedTrackID *int64 `json:"seed_track_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	sess := s.Play.StartSession(requestUserID(r), req.SeedTrackID)
	sess.Lock()
	defer sess.Unlock()
	writeJSON(w, s.sessionStartResponse(sess))
}

func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	sess := s.requireSession(w, r, "")
	if sess == nil {
		return
	}
	sess.Lock()
	defer sess.Unlock()
	out := map[string]any{
		"session_id": sess.ID,
		"mode":       sess.Mode,
		"maturity":   s.Play.MaturityOf(sess.UserID),
		"current":    s.trackJSON(sess.Current),
		"queue":      sess.Queue,
		"name":       sess.PlaylistName,
		"kind":       sess.PlaylistKind,
		"index":      sess.DailyPos,
		"count":      len(sess.DailyIDs),
		"fixed":      playback.IsFixedMode(sess.Mode),
	}
	if playback.IsFixedMode(sess.Mode) {
		out["tracks"] = s.sessionTracks(sess)
	}
	writeJSON(w, out)
}

func (s *Server) handleSessionJump(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Index     *int   `json:"index"`
		TrackID   int64  `json:"track_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	sess := s.requireSession(w, r, req.SessionID)
	if sess == nil {
		return
	}
	sess.Lock()
	defer sess.Unlock()
	if err := s.Play.Jump(sess, req.Index, req.TrackID); err != nil {
		writeErr(w, playHTTPStatus(err), "play", err.Error())
		return
	}
	out := s.playResponse(sess)
	out["ok"] = true
	writeJSON(w, out)
}

// handleSessionShuffle перемешивает фиксированный плейлист играющей
// сессии без пересоздания сессии и без прерывания текущего трека.
func (s *Server) handleSessionShuffle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	sess := s.requireSession(w, r, req.SessionID)
	if sess == nil {
		return
	}
	sess.Lock()
	defer sess.Unlock()
	if err := s.Play.ShuffleFixed(sess); err != nil {
		writeErr(w, playHTTPStatus(err), "play", err.Error())
		return
	}
	out := s.playResponse(sess)
	out["ok"] = true
	writeJSON(w, out)
}
