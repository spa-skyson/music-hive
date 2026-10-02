package playback

import (
	"sync"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/queue"
	"github.com/spa-skyson/music-hive/player/internal/taste"
)

type Engine struct {
	Cfg     config.Config
	Store   db.Backend
	Idx     index.Searcher
	Builder *queue.Builder

	Enqueue func(func())
	Flush   func()
	Observe func(string, time.Duration)
	Warm    func(*Session)

	sessionsMu  sync.RWMutex
	sessions    map[string]*Session
	transMu     sync.RWMutex
	transitions map[int64]map[int64]float64

	// tastes — in-memory EMA-профили по пользователям (F2.2): лениво
	// загружаются из Store при первом обращении, сбрасываются Reload'ом.
	tastesMu sync.Mutex
	tastes   map[int64]*taste.Profile
}

func New(cfg config.Config, store db.Backend, idx index.Searcher, builder *queue.Builder) *Engine {
	return &Engine{
		Cfg: cfg, Store: store, Idx: idx, Builder: builder,
		sessions: map[string]*Session{},
		tastes:   map[int64]*taste.Profile{},
	}
}

func (e *Engine) enqueue(work func()) {
	if work == nil {
		return
	}
	if e.Enqueue != nil {
		e.Enqueue(work)
		return
	}
	work()
}

func (e *Engine) flush() {
	if e.Flush != nil {
		e.Flush()
	}
}

func (e *Engine) observe(op string, d time.Duration) {
	if e.Observe != nil {
		e.Observe(op, d)
	}
}

func (e *Engine) warm(sess *Session) {
	if e.Warm != nil {
		e.Warm(sess)
	}
}

func (e *Engine) MaturityOf(userID int64) string {
	return e.TasteOf(userID).Maturity(e.Cfg.ProfileFormingAt, e.Cfg.ProfileReadyAt)
}

func (e *Engine) DiscoveringOf(userID int64) bool {
	return e.MaturityOf(userID) == taste.StatusDiscovering
}

func (e *Engine) ExploreOf(userID int64) float64 {
	return e.TasteOf(userID).EffectiveExplore(e.Cfg.ExploreRatio, e.Cfg.DiscoverExploreRatio,
		e.Cfg.ProfileFormingAt, e.Cfg.ProfileReadyAt)
}

// TasteOf — in-memory EMA-профиль пользователя (F2.2). Ленивая загрузка из
// Store: global-вектор + счётчики сигналов; без вектора — центроид каталога
// (семантика прежнего boot-load из app.Reload).
func (e *Engine) TasteOf(userID int64) *taste.Profile {
	e.tastesMu.Lock()
	defer e.tastesMu.Unlock()
	if p := e.tastes[userID]; p != nil {
		return p
	}
	p := taste.New()
	pos, neg, _ := e.Store.ListenSignalCounts(userID)
	if blob, err := e.Store.LatestProfile(userID, "global"); err == nil && len(blob) > 0 {
		v := index.BytesToFloat32(blob)
		if len(v) == e.Idx.Dim() {
			p.SetWithMeta(v, pos, neg, "online_ema")
		} else {
			p.SetCounts(pos, neg)
		}
	} else if !p.Ready() {
		p.SetWithMeta(e.Idx.Centroid(), pos, neg, "centroid")
	} else {
		p.SetCounts(pos, neg)
	}
	e.tastes[userID] = p
	return p
}

// ResetTastes сбрасывает кэш профилей (после reload индекса — размерность
// могла измениться, профили перечитаются лениво).
func (e *Engine) ResetTastes() {
	e.tastesMu.Lock()
	e.tastes = map[int64]*taste.Profile{}
	e.tastesMu.Unlock()
}

func (e *Engine) SessionCount() int {
	e.sessionsMu.RLock()
	defer e.sessionsMu.RUnlock()
	return len(e.sessions)
}

func (e *Engine) ReloadTransitions() {
	g, err := e.Store.LoadTransitionGraph()
	if err != nil {
		return
	}
	e.transMu.Lock()
	e.transitions = g
	e.transMu.Unlock()
}

func (e *Engine) BumpTransitionMem(from, to int64, w float64) {
	if from == 0 || to == 0 {
		return
	}
	e.transMu.Lock()
	defer e.transMu.Unlock()
	if e.transitions == nil {
		e.transitions = map[int64]map[int64]float64{}
	}
	m := e.transitions[from]
	if m == nil {
		m = map[int64]float64{}
		e.transitions[from] = m
	}
	m[to] += w
}
