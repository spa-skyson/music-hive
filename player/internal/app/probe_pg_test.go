package app

import (
	"fmt"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
	"github.com/spa-skyson/music-hive/player/internal/playback"
	"github.com/spa-skyson/music-hive/player/internal/queue"
)

// Живой PG-тест probe-цикла (#47): сид 2 готовых трека → загрузка →
// +1 готовый → ProbeOnce дёргает Reload и индекс растёт; без изменений в
// БД — Reload не дёргается (reloaded=false, err=nil — единственный путь к
// этой паре в ProbeOnce лежит до вызова Reload). Без
// MUSIC_HIVE_TEST_DATABASE_URL — skip.
func TestReloadProbeFollowsReadyCount(t *testing.T) {
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const dim = 4
	pgtest.ActivateModel(t, store.DB, "test:probe", dim, false)
	ready := func(i int) {
		t.Helper()
		id := pgtest.InsertTrack(t, store.DB, fmt.Sprintf("/music/p%d.flac", i),
			"Artist", "Album", fmt.Sprintf("P%d", i))
		pgtest.InsertEmbedding(t, store.DB, "test:probe", id, []float32{0.5, 0.5, 0.5, 0.5})
	}
	ready(1)
	ready(2)

	cfg := config.Config{ReloadProbeSec: 60}
	idx := index.NewPG(cfg, store.DB)
	play := playback.New(cfg, store, idx, queue.NewBuilder(idx, cfg))
	svc := New(cfg, store, idx, play, nil)
	if err := svc.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if idx.Size() != 2 {
		t.Fatalf("initial index size=%d want 2", idx.Size())
	}

	// Без изменений в БД: probe не дёргает reload.
	if reloaded, err := svc.ProbeOnce(); err != nil || reloaded {
		t.Fatalf("ProbeOnce without changes: reloaded=%v err=%v, want false/nil", reloaded, err)
	}

	// +1 готовый трек (инкрементальный прогресс длинной job) → reload.
	ready(3)
	reloaded, err := svc.ProbeOnce()
	if err != nil || !reloaded {
		t.Fatalf("ProbeOnce after new ready track: reloaded=%v err=%v, want true/nil", reloaded, err)
	}
	if idx.Size() != 3 {
		t.Fatalf("index size=%d want 3 after probe reload", idx.Size())
	}
}
