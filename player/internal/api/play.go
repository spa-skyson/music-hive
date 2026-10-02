package api

import (
	"encoding/json"
	"net/http"

	"github.com/spa-skyson/music-hive/player/internal/playback"
)

type playReq struct {
	TrackID      int64   `json:"track_id"`
	TrackIDs     []int64 `json:"track_ids"`
	Artist       string  `json:"artist"`
	Album        string  `json:"album"`
	StartIndex   *int    `json:"start_index"`
	StartTrackID int64   `json:"start_track_id"`
	Name         string  `json:"name"`
	Shuffle      bool    `json:"shuffle"`
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req playReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	startIdx := 0
	startTrackID := req.StartTrackID
	if req.Shuffle {
		// Перемешанный список всегда начинаем с начала: стартовая позиция
		// из несмешанного порядка рассинхронила бы ожидание клиента.
		startTrackID = 0
	} else if req.StartIndex != nil {
		startIdx = *req.StartIndex
	}
	ids, name, err := s.Play.ResolvePlayIDs(playback.PlaySpec{
		TrackID: req.TrackID, TrackIDs: req.TrackIDs,
		Artist: req.Artist, Album: req.Album, Name: req.Name,
		Shuffle: req.Shuffle,
	})
	if err != nil {
		writeErr(w, playHTTPStatus(err), "play", err.Error())
		return
	}
	sess := s.Play.StartFixed(requestUserID(r), ids, "listen", name, "listen", startIdx, startTrackID)
	sess.Lock()
	defer sess.Unlock()
	writeJSON(w, s.playResponse(sess))
}
