package library

import (
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// testIndex строит PGIndex поверх тестовой базы: группировки читают только
// метаданные (Size/MetaAt), векторы и активная модель здесь не нужны —
// Load со «своими» строками заполняет каталог напрямую.
func testIndex(t *testing.T, rows []db.TrackRow) *index.PGIndex {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	idx := index.NewPG(config.Config{}, store.DB)
	if err := idx.Load(rows); err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestMatchArtistAlbum(t *testing.T) {
	if !MatchArtistAlbum("Massive Attack", "Mezzanine", " massive attack ", "") {
		t.Fatal("artist match should be case-insensitive")
	}
	if MatchArtistAlbum("Massive Attack", "Protection", "Massive Attack", "mezzanine") {
		t.Fatal("album mismatch should fail")
	}
	if !MatchArtistAlbum("Portishead", "Dummy", "", "") {
		t.Fatal("empty filters should match")
	}
}

func TestGroupArtistsAndAlbums(t *testing.T) {
	idx := testIndex(t, []db.TrackRow{
		{ID: 1, Title: "One", Artist: "Massive Attack", Album: "Mezzanine", Dim: 2, ArtworkPath: "/a.png"},
		{ID: 2, Title: "Two", Artist: "Massive Attack", Album: "Protection", Dim: 2},
		{ID: 3, Title: "Three", Artist: "Portishead", Album: "Dummy", Dim: 2},
		{ID: 4, Title: "Four", Artist: "", Album: "", Dim: 2},
	})

	artists := GroupArtists(idx)
	if len(artists) != 3 {
		t.Fatalf("artists=%d, want 3: %+v", len(artists), artists)
	}
	if artists[0].Artist != "Massive Attack" || artists[0].Tracks != 2 || !artists[0].HasArtwork {
		t.Fatalf("top artist=%+v", artists[0])
	}

	albums := GroupAlbums(idx)
	if len(albums) != 3 {
		t.Fatalf("albums=%d, want 3 (empty album skipped): %+v", len(albums), albums)
	}
}
