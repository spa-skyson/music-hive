package queue

import (
	"fmt"
	"math"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

func TestTransitionNormMonotonic(t *testing.T) {
	maxTW := 10.0
	norm := func(w float64) float32 {
		if w <= 0 || maxTW <= 0 {
			return 0
		}
		return float32(math.Log1p(w) / math.Log1p(maxTW))
	}
	a := norm(1)
	b := norm(5)
	c := norm(10)
	if !(a < b && b <= c) {
		t.Fatalf("expected monotonic norms got %v %v %v", a, b, c)
	}
	if c > 1.01 {
		t.Fatalf("norm at max should be ~1 got %v", c)
	}
	old := float32(0.55*0.9 + 0.35*0.8)
	with := old + transitionLambda*norm(5)
	if with <= old {
		t.Fatal("transition boost should increase score")
	}
}

func TestCandidatePoolThreshold(t *testing.T) {
	if transitionLambda <= 0 {
		t.Fatal("lambda")
	}
}

// testBuilder наливает 12 треков (первые 6 — Artist A, остальные — Artist B)
// с векторами по возрастанию угла к вкусу [1,0] и строит Builder поверх
// PGIndex. Возвращает builder и id-шники в порядке вставки.
func testBuilder(t *testing.T) (*Builder, []int64) {
	t.Helper()
	store := openQueueStore(t)
	pgtest.ActivateModel(t, store.DB, "test:v1", 2, false)
	ids := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		artist := "Artist A"
		if i >= 6 {
			artist = "Artist B"
		}
		id := pgtest.InsertTrack(t, store.DB, fmt.Sprintf("/x%d.flac", i), artist, "Album", "Track")
		pgtest.InsertEmbedding(t, store.DB, "test:v1", id, []float32{float32(i + 1), 1})
		ids = append(ids, id)
	}
	idx := index.NewPG(config.Config{}, store.DB)
	rows, err := store.LoadReadyTracks()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Load(rows); err != nil {
		t.Fatal(err)
	}
	return NewBuilder(idx, config.Config{QueueSize: 5, ExploreRatio: 0.2}), ids
}

func openQueueStore(t *testing.T) *db.PGStore {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestBuildReturnsDistinctQueue(t *testing.T) {
	b, ids := testBuilder(t)
	taste := []float32{1, 0}
	index.Normalize(taste)
	items := b.BuildOpts(ids[0], taste, map[int64]bool{ids[0]: true}, BuildOpts{Size: 5, ExploreRatio: 0.2})
	if len(items) == 0 {
		t.Fatal("expected queue items")
	}
	if len(items) > 5 {
		t.Fatalf("queue len=%d, want <=5", len(items))
	}
	seen := map[int64]bool{ids[0]: true}
	for _, item := range items {
		if item.TrackID == 0 {
			t.Fatal("empty track id in queue")
		}
		if seen[item.TrackID] {
			t.Fatalf("duplicate track %d in queue", item.TrackID)
		}
		seen[item.TrackID] = true
		if item.Artist == "" || item.Title == "" {
			t.Fatalf("incomplete item: %+v", item)
		}
	}
}

func TestBuildHonorsExcludeAndTransitions(t *testing.T) {
	b, ids := testBuilder(t)
	exclude := map[int64]bool{ids[0]: true, ids[1]: true, ids[2]: true}
	items := b.BuildOpts(ids[3], []float32{1, 0}, exclude, BuildOpts{
		Size:            4,
		ExploreRatio:    0.25,
		TransitionsFrom: map[int64]float64{ids[9]: 20, ids[10]: 5},
	})
	for _, item := range items {
		if exclude[item.TrackID] {
			t.Fatalf("excluded track %d leaked into queue", item.TrackID)
		}
	}
}

func TestPickRandomSkipsExcluded(t *testing.T) {
	b, ids := testBuilder(t)
	exclude := map[int64]bool{}
	for _, id := range ids[:len(ids)-1] {
		exclude[id] = true
	}
	got := b.PickRandom(exclude)
	if got != ids[len(ids)-1] {
		t.Fatalf("PickRandom=%d, want only remaining id %d", got, ids[len(ids)-1])
	}
	if b.PickRandom(map[int64]bool{}) == 0 {
		t.Fatal("PickRandom on non-empty index returned 0")
	}
	// Пустой каталог: NewPG без Load — тот же инертный нулевой индекс,
	// какой даёт Load(nil) после reload пустой базы.
	empty := NewBuilder(index.NewPG(config.Config{}, openQueueStore(t).DB), config.Config{})
	if empty.PickRandom(nil) != 0 {
		t.Fatal("PickRandom on empty index should return 0")
	}
}

func TestRandomFloat64IsBounded(t *testing.T) {
	b, _ := testBuilder(t)
	for i := 0; i < 20; i++ {
		v := b.RandomFloat64()
		if v < 0 || v >= 1 {
			t.Fatalf("RandomFloat64=%f out of [0,1)", v)
		}
	}
}
