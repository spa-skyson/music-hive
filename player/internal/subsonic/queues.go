// Play-queue и now-playing Subsonic (F3.4, GitLab #26): savePlayQueue/
// getPlayQueue ↔ play_sessions, getNowPlaying — активные сессии плеера.
// Одна очередь на пользователя: строка play_sessions с фиксированным id
// "subsonic-<userID>" (mode='subsonic'), состояние — JSON в существующем
// queue_json (новые колонки/таблицы запрещены задачей).
// ponytail: строка живёт по общим правилам GC play_sessions — очередь
// забудется, если не обновлять её ~7 дней или при вытеснении из 64
// последних сессий; отдельное хранилище — если клиенты начнут жаловаться.

package subsonic

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// ---------------------------------------------------------------- DTO

// PlayQueue — ответ getPlayQueue: current — id текущего трека, position —
// смещение в мс, changed/changedBy — время и клиент последнего сохранения.
type PlayQueue struct {
	Username  string       `xml:"username,attr" json:"username"`
	Position  int64        `xml:"position,attr" json:"position"`
	Changed   string       `xml:"changed,attr" json:"changed"`
	ChangedBy string       `xml:"changedBy,attr" json:"changedBy"`
	Current   string       `xml:"current,attr" json:"current"`
	Entry     Array[Child] `xml:"entry" json:"entry"`
}

// NowPlaying — ответ getNowPlaying.
type NowPlaying struct {
	Entry Array[NowPlayingEntry] `xml:"entry" json:"entry"`
}

// NowPlayingEntry — трек играющей сессии (+ кто и когда).
type NowPlayingEntry struct {
	Child
	Username   string `xml:"username,attr" json:"username"`
	MinutesAgo int    `xml:"minutesAgo,attr" json:"minutesAgo"`
	PlayerID   int    `xml:"playerId,attr" json:"playerId"`
}

// ---------------------------------------------------------------- окно Store

// SavedQueue — сохранённая subsonic-очередь пользователя.
type SavedQueue struct {
	IDs       []int64
	Current   int64
	Position  int64 // мс внутри текущего трека
	Changed   time.Time
	ChangedBy string // имя клиента (параметр c)
}

// NowPlayingRow — активная сессия плеера (последние ~10 минут).
type NowPlayingRow struct {
	Username  string
	TrackID   int64
	UpdatedAt time.Time
}

// QueueStore — доступ к play_sessions для очереди и now-playing
// (реализация — *db.PGStore поверх существующих таблиц без миграций).
type QueueStore interface {
	SubsonicSaveQueue(userID int64, q SavedQueue) error
	SubsonicLoadQueue(userID int64) (SavedQueue, bool, error)
	SubsonicNowPlaying() ([]NowPlayingRow, error)
}

// ---------------------------------------------------------------- handlers

// savePlayQueue — songId[] + currentIndex + position (мс) + changed;
// треки проверяются по каталогу (70), как в остальных мульти-id эндпоинтах.
func (rt *Router) savePlayQueue(r *http.Request) (*Subsonic, error) {
	if rt.Queues == nil || rt.Catalog == nil {
		return nil, notImplemented("savePlayQueue")
	}
	q := r.URL.Query()
	ids, err := parseSongIDs(q["songId"])
	if err != nil || len(ids) == 0 {
		if err == nil {
			err = NewError(ErrorMissingParameter, "required parameter songId is missing")
		}
		return nil, err
	}
	if err := rt.checkSongs(ids); err != nil {
		return nil, err
	}
	current := ids[0]
	if idx, err := strconv.Atoi(q.Get("currentIndex")); err == nil && idx >= 0 && idx < len(ids) {
		current = ids[idx]
	}
	var position int64
	if v, err := strconv.ParseInt(q.Get("position"), 10, 64); err == nil && v > 0 {
		position = v
	}
	changed := time.Now().UTC()
	if t, err := time.Parse(time.RFC3339, q.Get("changed")); err == nil {
		changed = t.UTC()
	}
	sq := SavedQueue{
		IDs: ids, Current: current, Position: position,
		Changed: changed, ChangedBy: q.Get("c"),
	}
	if err := rt.Queues.SubsonicSaveQueue(requestUser(r).ID, sq); err != nil {
		return nil, err
	}
	return rt.NewResponse(), nil
}

// getPlayQueue — сохранённая очередь; ничего не сохранено → пустой ok
// (клиент стартует с новой очереди).
func (rt *Router) getPlayQueue(r *http.Request) (*Subsonic, error) {
	if rt.Queues == nil || rt.Catalog == nil {
		return nil, notImplemented("getPlayQueue")
	}
	sq, found, err := rt.Queues.SubsonicLoadQueue(requestUser(r).ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return rt.NewResponse(), nil
	}
	out := &PlayQueue{
		Username:  requestUser(r).Username,
		Position:  sq.Position,
		Changed:   isoTime(sq.Changed),
		ChangedBy: sq.ChangedBy,
		Entry:     Array[Child]{},
	}
	if sq.Current != 0 {
		out.Current = strconv.FormatInt(sq.Current, 10)
	}
	for _, id := range sq.IDs {
		song, ok, err := rt.Catalog.SubsonicSong(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // трек исчез из каталога после сохранения
		}
		out.Entry = append(out.Entry, songChild(song, strconv.FormatInt(song.AlbumID, 10)))
	}
	resp := rt.NewResponse()
	resp.PlayQueue = out
	return resp, nil
}

// getNowPlaying — сессии плеера, обновлённые за последние ~10 минут
// (subsonic-строки очередей не считаются «играющими»).
func (rt *Router) getNowPlaying(_ *http.Request) (*Subsonic, error) {
	if rt.Queues == nil || rt.Catalog == nil {
		return nil, notImplemented("getNowPlaying")
	}
	rows, err := rt.Queues.SubsonicNowPlaying()
	if err != nil {
		return nil, err
	}
	out := make(Array[NowPlayingEntry], 0, len(rows))
	for _, row := range rows {
		song, ok, err := rt.Catalog.SubsonicSong(row.TrackID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		child := songChild(song, strconv.FormatInt(song.AlbumID, 10))
		out = append(out, NowPlayingEntry{
			Child:      child,
			Username:   row.Username,
			MinutesAgo: int(time.Since(row.UpdatedAt).Minutes()),
			PlayerID:   0, // отдельных плееров нет — один на пользователя
		})
	}
	resp := rt.NewResponse()
	resp.NowPlaying = &NowPlaying{Entry: out}
	return resp, nil
}

// MarshalQueueJSON — формат queue_json subsonic-строки (единый источник
// правды про очередь; play_sessions.current_id дублирует текущий трек).
func MarshalQueueJSON(q SavedQueue) (string, error) {
	blob := struct {
		IDs       []int64 `json:"ids"`
		Position  int64   `json:"position"`
		Changed   string  `json:"changed"`
		ChangedBy string  `json:"changedBy"`
	}{q.IDs, q.Position, isoTime(q.Changed), q.ChangedBy}
	b, err := json.Marshal(blob)
	return string(b), err
}

// UnmarshalQueueJSON — обратное чтение (Changed — из blob, RFC3339).
func UnmarshalQueueJSON(raw string) (SavedQueue, error) {
	var blob struct {
		IDs       []int64 `json:"ids"`
		Position  int64   `json:"position"`
		Changed   string  `json:"changed"`
		ChangedBy string  `json:"changedBy"`
	}
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return SavedQueue{}, err
	}
	changed := time.Time{}
	if t, err := time.Parse(time.RFC3339, blob.Changed); err == nil {
		changed = t
	}
	return SavedQueue{IDs: blob.IDs, Position: blob.Position, Changed: changed, ChangedBy: blob.ChangedBy}, nil
}
