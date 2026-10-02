package recommend

import (
	"fmt"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// similarIndex наливает 6 треков с векторами в тестовую базу и строит
// поверх них PGIndex. Возвращает store и id-шники в порядке seeds:
// ids[0] и ids[5] — клоны (один file_md5, один artist|title).
func similarIndex(t *testing.T) (*index.PGIndex, *db.PGStore, []int64) {
	t.Helper()
	store := openRecommendStore(t)
	pgtest.ActivateModel(t, store.DB, "test:v1", 2, false)
	seeds := []struct {
		artist, album, title string
		vec                  []float32
		md5                  string
	}{
		{"Massive Attack", "Mezzanine", "Angel", []float32{1, 0}, "md5-a"},
		{"Massive Attack", "Mezzanine", "Teardrop", []float32{0.95, 0.05}, ""},
		{"Portishead", "Dummy", "Glory Box", []float32{0.9, 0.1}, ""},
		{"Portishead", "Third", "Machine Gun", []float32{0.2, 0.8}, ""},
		{"Bjork", "Homogenic", "Joga", []float32{0, 1}, ""},
		{"Massive Attack", "Mezzanine", "Angel", []float32{0.99, 0.01}, "md5-a"},
	}
	ids := make([]int64, 0, len(seeds))
	for i, s := range seeds {
		id := pgtest.InsertTrack(t, store.DB, fmt.Sprintf("/t%d.flac", i), s.artist, s.album, s.title)
		if s.md5 != "" {
			if _, err := store.DB.Exec(`UPDATE tracks SET file_md5 = $1 WHERE id = $2`, s.md5, id); err != nil {
				t.Fatal(err)
			}
		}
		pgtest.InsertEmbedding(t, store.DB, "test:v1", id, s.vec)
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
	return idx, store, ids
}

func TestSimilarTracksExcludesClones(t *testing.T) {
	idx, _, ids := similarIndex(t)
	hits := SimilarTracks(idx, ids[0], 10)
	if len(hits) == 0 {
		t.Fatal("expected similar tracks")
	}
	for _, h := range hits {
		if h.ID == ids[0] || h.ID == ids[5] {
			t.Fatalf("clone id %d should be excluded: %+v", h.ID, hits)
		}
	}
	if hits[0].ID != ids[1] && hits[0].ID != ids[2] {
		t.Fatalf("top similar = %d, want near Mezzanine/Portishead", hits[0].ID)
	}
}

func TestSimilarArtistsAndAlbums(t *testing.T) {
	idx, _, _ := similarIndex(t)
	artists := SimilarArtists(idx, "Massive Attack", 12)
	if len(artists) == 0 {
		t.Fatal("expected similar artists")
	}
	for _, a := range artists {
		if a.Artist == "Massive Attack" {
			t.Fatal("seed artist must be excluded")
		}
	}
	if artists[0].Artist != "Portishead" {
		t.Fatalf("top artist=%q, want Portishead", artists[0].Artist)
	}

	albums := SimilarAlbums(idx, "Massive Attack", "Mezzanine", 12)
	if len(albums) == 0 {
		t.Fatal("expected similar albums")
	}
	for _, a := range albums {
		if a.Album == "Mezzanine" && a.Artist == "Massive Attack" {
			t.Fatal("seed album must be excluded")
		}
	}
}

func TestFromTrackAndArtistExcludeSeed(t *testing.T) {
	idx, _, ids := similarIndex(t)
	tracks := FromTrack(idx, ids[0], 3)
	if len(tracks) == 0 {
		t.Fatal("expected recommendations from track")
	}
	for _, tr := range tracks {
		if tr.ID == ids[0] {
			t.Fatal("seed track must be excluded")
		}
	}

	tracks = FromArtist(idx, "Bjork", 20)
	for _, tr := range tracks {
		if tr.ID == ids[4] || tr.Artist == "Bjork" {
			t.Fatalf("seed artist tracks leaked: %+v", tr)
		}
	}
}

func TestFromFavoritesEmpty(t *testing.T) {
	idx, store, _ := similarIndex(t)
	mix := FromFavorites(store, idx, store.UserID())
	if !mix.Empty {
		t.Fatalf("expected empty mix without hearts: %+v", mix)
	}
}

func TestFromFavoritesUsesHearts(t *testing.T) {
	idx, store, ids := similarIndex(t)
	owner := store.UserID()
	if err := store.FavoritesAdd(owner, ids[0]); err != nil {
		t.Fatal(err)
	}
	mix := FromFavorites(store, idx, owner)
	if mix.Empty {
		t.Fatal("expected mix from favorite track")
	}
	for _, tr := range mix.Tracks {
		if tr.ID == ids[0] {
			t.Fatal("favorite seed track must be excluded")
		}
	}
}

func openRecommendStore(t *testing.T) *db.PGStore {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
