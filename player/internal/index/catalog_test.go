package index

// Живой PG-тест прямого контракта клон-детекции каталога (#49): группа
// клонов трека — объединение дубликатов по file_md5 и по SongKey
// (нормализованное artist|title). Прямой тест жил в удалённом RAM
// index_test.go (!17), логика переехала в buildCatalog/PGIndex.CloneIDs —
// здесь ряды из старого теста воспроизведены на PG-пути.

import (
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

func TestPGCloneIDsIncludesMD5AndSongKeyDuplicates(t *testing.T) {
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const dim = 2
	pgtest.ActivateModel(t, store.DB, "test:v1", dim, false)

	// InsertTrack генерирует file_md5 из пути — для общих md5 правим вручную.
	insert := func(path, artist, title, md5 string) int64 {
		id := pgtest.InsertTrack(t, store.DB, path, artist, "Album", title)
		if _, err := store.DB.Exec(
			`UPDATE tracks SET file_md5 = $1 WHERE id = $2`, md5, id); err != nil {
			t.Fatalf("set file_md5: %v", err)
		}
		pgtest.InsertEmbedding(t, store.DB, "test:v1", id, []float32{1, 0})
		return id
	}
	// Клоны трека id1: id2 — по SongKey (same|song), id3 — по file_md5 abc;
	// id4 — посторонний трек, в группу не входит.
	id1 := insert("/music/clone-a.flac", "Same", "Song", "abc")
	id2 := insert("/music/clone-b.flac", "Same", "Song", "other")
	id3 := insert("/music/clone-c.flac", "Other", "Track", "abc")
	id4 := insert("/music/clone-d.flac", "Solo", "Alone", "zzz")

	idx := NewPG(config.Config{}, store.DB)
	rows, err := store.LoadReadyTracks()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Load(rows); err != nil {
		t.Fatal(err)
	}

	got := idx.CloneIDs(id1)
	seen := map[int64]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, want := range []int64{id1, id2, id3} {
		if !seen[want] {
			t.Fatalf("CloneIDs(%d)=%v, missing %d", id1, got, want)
		}
	}
	if seen[id4] {
		t.Fatalf("CloneIDs(%d)=%v unexpectedly includes unrelated id %d", id1, got, id4)
	}
	if got := idx.CloneIDs(99); len(got) != 1 || got[0] != 99 {
		t.Fatalf("missing track CloneIDs=%v, want [99]", got)
	}
}
