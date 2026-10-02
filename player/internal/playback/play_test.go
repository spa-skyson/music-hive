package playback

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/queue"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// testPGEngine — тестовый движок на живом PG: треки
// 11/22/33 с теми же эмбеддингами test:playback dim 2, owner-пользователь
// вместо литерала 0 (см. store.UserID()).
func testPGEngine(t *testing.T) (*Engine, int64) {
	t.Helper()
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pgtest.ActivateModel(t, store.DB, "test:playback", 2, false)
	tracks := []struct {
		id     int64
		title  string
		vector []float32
	}{
		{11, "One", []float32{1, 11}},
		{22, "Two", []float32{1, 22}},
		{33, "Three", []float32{1, 33}},
	}
	for _, tc := range tracks {
		if _, err := store.DB.Exec(
			`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
			 VALUES ($1,$2,$3,'Artist','Album',180)`,
			tc.id, "/track-"+itoa(tc.id)+".flac", tc.title,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB.Exec(
		`SELECT setval(pg_get_serial_sequence('tracks','id'), (SELECT MAX(id) FROM tracks))`,
	); err != nil {
		t.Fatal(err)
	}
	for _, tc := range tracks {
		pgtest.InsertEmbedding(t, store.DB, "test:playback", tc.id, tc.vector)
	}

	cfg := config.Config{DatabaseURL: dsn, QueueSize: 6, ProfileFormingAt: 3, ProfileReadyAt: 8, ExploreRatio: 0.15}
	idx := index.NewPG(cfg, store.DB)
	rows, err := store.LoadReadyTracks()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Load(rows); err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, idx, queue.NewBuilder(idx, cfg)), store.UserID()
}

func TestStartFixedPreservesOrderAndStartPosition(t *testing.T) {
	tests := []struct {
		name         string
		startIndex   int
		startTrackID int64
		wantIndex    int
		wantCurrent  int64
	}{
		{name: "index", startIndex: 2, wantIndex: 2, wantCurrent: 33},
		{name: "track id overrides index", startIndex: 0, startTrackID: 22, wantIndex: 1, wantCurrent: 22},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, owner := testPGEngine(t)
			order := []int64{11, 22, 33}
			session := engine.StartFixed(
				owner, append([]int64(nil), order...), "playlist", "Ordered", "daily",
				test.startIndex, test.startTrackID,
			)
			if !reflect.DeepEqual(session.DailyIDs, order) {
				t.Fatalf("session order = %v, want %v", session.DailyIDs, order)
			}
			if session.DailyPos != test.wantIndex || session.Current != test.wantCurrent {
				t.Fatalf("position/current = %d/%d, want %d/%d",
					session.DailyPos, session.Current, test.wantIndex, test.wantCurrent)
			}

			row, ok, err := engine.Store.LoadPlaySession(owner, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("fixed session was not persisted")
			}
			var persistedOrder []int64
			if err := json.Unmarshal([]byte(row.DailyIDsJSON), &persistedOrder); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persistedOrder, order) {
				t.Fatalf("persisted order = %v, want %v", persistedOrder, order)
			}
			if row.DailyPos != test.wantIndex || row.CurrentID != test.wantCurrent {
				t.Fatalf("persisted position/current = %d/%d, want %d/%d",
					row.DailyPos, row.CurrentID, test.wantIndex, test.wantCurrent)
			}
		})
	}
}

func TestResolvePlayIDsByArtist(t *testing.T) {
	engine, _ := testPGEngine(t)
	ids, name, err := engine.ResolvePlayIDs(PlaySpec{Artist: "Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "Artist" || !reflect.DeepEqual(ids, []int64{11, 22, 33}) {
		t.Fatalf("ids=%v name=%q", ids, name)
	}
}

// TestResolvePlayIDsShuffleKeepsSetAndChangesOrder: shuffle сохраняет
// множество id без потерь и дублей и меняет порядок канонического списка.
func TestResolvePlayIDsShuffleKeepsSetAndChangesOrder(t *testing.T) {
	engine, _ := testPGEngine(t)

	// Отдельное соединение к той же БД: engine.Store — интерфейс db.Backend.
	store, err := db.OpenPG(engine.Cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 20 треков отдельного артиста: на таком списке случайное совпадение
	// с каноническим порядком (1/20!) практически невозможно.
	const extra = 20
	for id := int64(1000); id < 1000+extra; id++ {
		if _, err := store.DB.Exec(
			`INSERT INTO tracks(id, path, title, artist, album, duration) OVERRIDING SYSTEM VALUE
			 VALUES ($1,$2,'Track','Shuffle Artist','Album',180)`,
			id, "/shuffle-"+itoa(id)+".flac",
		); err != nil {
			t.Fatal(err)
		}
		pgtest.InsertEmbedding(t, store.DB, "test:playback", id, []float32{1, float32(id)})
	}
	rows, err := store.LoadReadyTracks()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Idx.Load(rows); err != nil {
		t.Fatal(err)
	}

	canonical, _, err := engine.ResolvePlayIDs(PlaySpec{Artist: "Shuffle Artist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) != extra {
		t.Fatalf("canonical len = %d, want %d", len(canonical), extra)
	}
	wantSet := append([]int64(nil), canonical...)
	slices.Sort(wantSet)

	// Несколько попыток: достаточно одной неудачи совпадения с каноном.
	shuffled := false
	for attempt := 0; attempt < 5 && !shuffled; attempt++ {
		ids, _, err := engine.ResolvePlayIDs(PlaySpec{Artist: "Shuffle Artist", Shuffle: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != extra {
			t.Fatalf("shuffled len = %d, want %d", len(ids), extra)
		}
		gotSet := append([]int64(nil), ids...)
		slices.Sort(gotSet)
		if !slices.Equal(gotSet, wantSet) {
			t.Fatalf("shuffle changed the set: got %v, want %v", gotSet, wantSet)
		}
		if !slices.Equal(ids, canonical) {
			shuffled = true
		}
	}
	if !shuffled {
		t.Fatal("shuffle kept canonical order in 5 attempts")
	}
}

// TestShuffleFixedKeepsCurrentFirst: ShuffleFixed оставляет текущий трек
// играть на позиции 0 и сохраняет множество DailyIDs.
func TestShuffleFixedKeepsCurrentFirst(t *testing.T) {
	engine, owner := testPGEngine(t)
	order := []int64{11, 22, 33}
	session := engine.StartFixed(owner, append([]int64(nil), order...), "playlist", "Ordered", "daily", 2, 0)
	if session.Current != 33 {
		t.Fatalf("current = %d, want 33", session.Current)
	}

	if err := engine.ShuffleFixed(session); err != nil {
		t.Fatal(err)
	}
	if session.Current != 33 || session.DailyPos != 0 || session.DailyIDs[0] != 33 {
		t.Fatalf("current/pos/first = %d/%d/%d, want 33/0/33",
			session.Current, session.DailyPos, session.DailyIDs[0])
	}
	gotSet := append([]int64(nil), session.DailyIDs...)
	slices.Sort(gotSet)
	if !slices.Equal(gotSet, []int64{11, 22, 33}) {
		t.Fatalf("DailyIDs = %v, want set {11 22 33}", session.DailyIDs)
	}
	if len(session.Queue) != 2 {
		t.Fatalf("queue len = %d, want 2", len(session.Queue))
	}

	// Радио-сессия не фиксированная — перемешивать нечего.
	radio := engine.StartRadio(owner, nil)
	if err := engine.ShuffleFixed(radio); err == nil {
		t.Fatal("expected error for non-fixed session")
	}
}

func TestStartShareInitializesSession(t *testing.T) {
	engine, owner := testPGEngine(t)

	session := engine.StartShare(owner)
	current := engine.ShareCurrentTrackID(session)

	if session.Mode != "share" {
		t.Fatalf("mode = %q, want share", session.Mode)
	}
	if current == 0 {
		t.Fatal("current track was not selected")
	}
	if !session.Exclude[current] {
		t.Fatalf("current track %d was not excluded", current)
	}
	if len(session.Queue) == 0 {
		t.Fatal("share queue was not initialized")
	}
}

func TestAdvanceShareRestartsAfterAdvanceEnds(t *testing.T) {
	engine, owner := testPGEngine(t)
	session := engine.NewSession(owner, "listen")
	session.Lock()
	session.Current = 11
	session.DailyIDs = []int64{11}
	session.DailyPos = 0
	session.Unlock()

	next := engine.AdvanceShare(session)

	if next == 0 {
		t.Fatal("share fallback did not select a track")
	}
	if session.Mode != "share" {
		t.Fatalf("mode = %q, want share", session.Mode)
	}
	if session.Current != next {
		t.Fatalf("current = %d, want %d", session.Current, next)
	}
	if !session.Exclude[next] {
		t.Fatalf("fallback track %d was not excluded", next)
	}
}
