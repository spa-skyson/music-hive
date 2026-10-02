// Медиа-эндпоинты Subsonic (F3.3, GitLab #25): stream/download/getCoverArt.
// Бинарные ответы (не конверт): аудио и обложки отдаются существующим
// media-слоем /api/stream (ServeMobile/ServeOriginal/ServeArtworkFile) —
// логика стриминга/транскода/Range не дублируется. DTO getCoverArt-идов
// производны от Navidrome (https://github.com/navidrome/navidrome),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"net/http"
	"strconv"
	"strings"
)

// stream — аудиофайл трека. Транскод: format≠raw при явном формате, либо
// maxBitRate ниже родного битрейта — отдаётся мобильный кеш-вариант
// (cfg.MobileFormat/MobileBitrate, как /api/stream?q=mobile). stream не
// пишет plays — за историю отвечает scrobble. timeOffset — только для
// видео по спецификации, игнорируем.
func (rt *Router) stream(w http.ResponseWriter, r *http.Request) {
	song, ok := rt.streamLookup(w, r)
	if !ok {
		return
	}
	if rt.Media == nil {
		rt.sendError(w, r, notImplemented("stream"))
		return
	}
	q := r.URL.Query()
	maxBitRate, _ := strconv.Atoi(q.Get("maxBitRate"))
	format := strings.ToLower(q.Get("format"))
	// ponytail: транскод всегда в мобильный профиль, а не в запрошенный
	// формат/битрейт; отдельные профили — когда клиенты реально попросят.
	needTranscode := format != "" && format != "raw" ||
		maxBitRate > 0 && song.Bitrate > maxBitRate
	if needTranscode {
		rt.Media.ServeMobile(w, r, song.ID, song.Path)
		return
	}
	rt.Media.ServeOriginal(w, r, song.Path)
}

// download — оригинал файла без транскода.
func (rt *Router) download(w http.ResponseWriter, r *http.Request) {
	song, ok := rt.streamLookup(w, r)
	if !ok {
		return
	}
	if rt.Media == nil {
		rt.sendError(w, r, notImplemented("download"))
		return
	}
	rt.Media.ServeOriginal(w, r, song.Path)
}

// streamLookup — общий id→трек для stream/download: 10/70 уходят конвертом.
func (rt *Router) streamLookup(w http.ResponseWriter, r *http.Request) (SongRow, bool) {
	if rt.Catalog == nil {
		rt.sendError(w, r, notImplemented("stream"))
		return SongRow{}, false
	}
	id, err := parseEntityID(r, "id")
	if err != nil {
		rt.sendError(w, r, err)
		return SongRow{}, false
	}
	song, found, err := rt.Catalog.SubsonicSong(id)
	if err != nil {
		rt.sendError(w, r, err)
		return SongRow{}, false
	}
	if !found {
		rt.sendError(w, r, NewError(ErrorDataNotFound, ""))
		return SongRow{}, false
	}
	return song, true
}

// getCoverArt — обложка по id из coverArt-полей: al-<albumID> (cover_track_id
// альбома, иначе первый трек с artwork), ar-<artistID> (обложка первого
// альбома), mf-<trackID>. size — ресайз существующим artwork-механизмом.
// Нет обложки/сущности — конверт с кодом 70.
func (rt *Router) getCoverArt(w http.ResponseWriter, r *http.Request) {
	if rt.Catalog == nil || rt.Media == nil {
		rt.sendError(w, r, notImplemented("getCoverArt"))
		return
	}
	raw := r.URL.Query().Get("id")
	if raw == "" {
		rt.sendError(w, r, NewError(ErrorMissingParameter, "required parameter id is missing"))
		return
	}
	kind := "mf" // голый числовой id трактуем как трек (толерантность к клиентам)
	if rest, ok := strings.CutPrefix(raw, "al-"); ok {
		kind, raw = "al", rest
	} else if rest, ok := strings.CutPrefix(raw, "ar-"); ok {
		kind, raw = "ar", rest
	} else if rest, ok := strings.CutPrefix(raw, "mf-"); ok {
		kind, raw = "mf", rest
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		rt.sendError(w, r, NewError(ErrorDataNotFound, ""))
		return
	}
	trackID, artworkPath, found, err := rt.Catalog.SubsonicCoverTrack(kind, id)
	if err != nil {
		rt.sendError(w, r, err)
		return
	}
	if !found || !rt.Media.ServeArtworkFile(w, r, trackID, artworkPath, sizeParam(r)) {
		rt.sendError(w, r, NewError(ErrorDataNotFound, ""))
	}
}

// sizeParam — size в пикселях; мусор = 0 (оригинал).
func sizeParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
