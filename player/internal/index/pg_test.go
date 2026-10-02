package index

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// Живые PG-тесты векторного индекса: MUSIC_HIVE_TEST_DATABASE_URL, иначе skip.
// Модель в тестах активируется без HNSW-индекса: на малых N планировщик делает
// seq scan и <=> даёт точный результат — проверяем порядок и дистанции строго.

func openPGIndex(t *testing.T) (*PGIndex, *db.PGStore, []int64, [][]float32, *rand.Rand) {
	t.Helper()
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const dim = 16
	const n = 60
	pgtest.ActivateModel(t, store.DB, "test:v1", dim, false)

	rng := rand.New(rand.NewSource(42))
	ids := make([]int64, 0, n)
	vecs := make([][]float32, 0, n)
	for i := 0; i < n; i++ {
		id := pgtest.InsertTrack(t, store.DB, fmt.Sprintf("/music/v%d.flac", i),
			// artist — функция альбома: иначе пары (artist, album) уникальны
			// и группировка по паре (RowsForAlbum) не проверяется.
			fmt.Sprintf("Artist %d", (i%13)%7), fmt.Sprintf("Album %d", i%13), fmt.Sprintf("V%d", i))
		vec := make([]float32, dim)
		var norm float64
		for d := range vec {
			vec[d] = rng.Float32() - 0.5
			norm += float64(vec[d]) * float64(vec[d])
		}
		inv := float32(1 / math.Sqrt(norm))
		for d := range vec {
			vec[d] *= inv
		}
		pgtest.InsertEmbedding(t, store.DB, "test:v1", id, vec)
		ids = append(ids, id)
		vecs = append(vecs, vec)
	}
	idx := NewPG(config.Config{NewTrackDays: 14, NewBoostBeta: 0.25}, store.DB)
	rows, err := store.LoadReadyTracks()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Load(rows); err != nil {
		t.Fatal(err)
	}
	if idx.Size() != n || idx.Dim() != dim {
		t.Fatalf("index size=%d dim=%d want %d/%d", idx.Size(), idx.Dim(), n, dim)
	}
	return idx, store, ids, vecs, rng
}

// bruteForce — эталонный топ-K в Go по тем же векторам.
func bruteForce(vecs [][]float32, query []float32, k int, exclude map[int64]bool, ids []int64) []ScoredRow {
	type pair struct {
		i   int
		sim float32
	}
	all := make([]pair, 0, len(vecs))
	for i, v := range vecs {
		if exclude != nil && exclude[ids[i]] {
			continue
		}
		var s float32
		for d := range v {
			s += v[d] * query[d]
		}
		all = append(all, pair{i, s})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].sim == all[b].sim {
			return all[a].i < all[b].i
		}
		return all[a].sim > all[b].sim
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([]ScoredRow, 0, len(all))
	for _, p := range all {
		out = append(out, ScoredRow{Row: p.i, Score: p.sim})
	}
	return out
}

func TestPGIndexTopKMatchesBruteForce(t *testing.T) {
	idx, _, ids, vecs, rng := openPGIndex(t)

	for round := 0; round < 5; round++ {
		query := make([]float32, idx.Dim())
		var norm float64
		for d := range query {
			query[d] = rng.Float32() - 0.5
			norm += float64(query[d]) * float64(query[d])
		}
		inv := float32(1 / math.Sqrt(norm))
		for d := range query {
			query[d] *= inv
		}

		want := bruteForce(vecs, query, 5, nil, ids)
		got := idx.TopK(query, 5, nil)
		if len(got) != len(want) {
			t.Fatalf("round %d: TopK len=%d want %d", round, len(got), len(want))
		}
		for i := range want {
			if got[i].Row != want[i].Row {
				t.Fatalf("round %d: TopK[%d] row=%d want %d", round, i, got[i].Row, want[i].Row)
			}
			if math.Abs(float64(got[i].Score-want[i].Score)) > 1e-5 {
				t.Fatalf("round %d: TopK[%d] score=%f want %f", round, i, got[i].Score, want[i].Score)
			}
		}

		// с исключением: выкинуть треть строк
		exclude := map[int64]bool{}
		for i := 0; i < len(ids); i += 3 {
			exclude[ids[i]] = true
		}
		want = bruteForce(vecs, query, 4, exclude, ids)
		got = idx.TopK(query, 4, exclude)
		if len(got) != len(want) {
			t.Fatalf("round %d: TopK(excl) len=%d want %d", round, len(got), len(want))
		}
		for i := range want {
			if got[i].Row != want[i].Row {
				t.Fatalf("round %d: TopK(excl)[%d] row=%d want %d", round, i, got[i].Row, want[i].Row)
			}
		}
	}

	// Vector: строка возвращает исходный вектор
	row, _ := idx.RowOf(ids[7])
	v := idx.Vector(row)
	if len(v) != idx.Dim() {
		t.Fatalf("Vector len=%d", len(v))
	}
	for d := range v {
		if math.Abs(float64(v[d]-vecs[7][d])) > 1e-6 {
			t.Fatalf("Vector mismatch at %d: %f vs %f", d, v[d], vecs[7][d])
		}
	}
}

func TestPGIndexSimsCentroidCandidates(t *testing.T) {
	idx, _, ids, vecs, _ := openPGIndex(t)

	query := make([]float32, idx.Dim())
	var norm float64
	for d := range query {
		query[d] = 0.25
		norm += 0.0625
	}
	inv := float32(1 / math.Sqrt(norm))
	for d := range query {
		query[d] *= inv
	}

	// SimsTo: точные близости всех строк
	sims := idx.SimsTo(query)
	if len(sims) != idx.Size() {
		t.Fatalf("SimsTo len=%d want %d", len(sims), idx.Size())
	}
	for i, v := range vecs {
		var want float32
		for d := range v {
			want += v[d] * query[d]
		}
		if math.Abs(float64(sims[i]-want)) > 1e-5 {
			t.Fatalf("SimsTo[%d]=%f want %f", i, sims[i], want)
		}
	}

	// SimsFor: подмножество, остальные нули
	subset := []int{3, 9, 41}
	subs := idx.SimsFor(query, subset)
	for _, i := range subset {
		var want float32
		for d := range vecs[i] {
			want += vecs[i][d] * query[d]
		}
		if math.Abs(float64(subs[i]-want)) > 1e-5 {
			t.Fatalf("SimsFor[%d]=%f want %f", i, subs[i], want)
		}
	}
	if subs[0] != 0 || subs[1] != 0 {
		t.Fatal("SimsFor must be zero outside requested rows")
	}

	// CentroidOf: нормированное среднее выбранных векторов
	wantMean := make([]float64, idx.Dim())
	for _, i := range subset {
		for d := range vecs[i] {
			wantMean[d] += float64(vecs[i][d])
		}
	}
	var wn float64
	for d := range wantMean {
		wantMean[d] /= float64(len(subset))
		wn += wantMean[d] * wantMean[d]
	}
	gotCentroid := idx.CentroidOf(subset)
	if len(gotCentroid) != idx.Dim() {
		t.Fatalf("CentroidOf len=%d", len(gotCentroid))
	}
	invN := 1 / math.Sqrt(wn)
	for d := range gotCentroid {
		if math.Abs(float64(gotCentroid[d])-wantMean[d]*invN) > 1e-5 {
			t.Fatalf("CentroidOf[%d]=%f want %f", d, gotCentroid[d], float32(wantMean[d]*invN))
		}
	}

	// Centroid: среднее по всей библиотеке (ненулевой, единичная норма)
	all := idx.Centroid()
	var n2 float64
	for _, x := range all {
		n2 += float64(x) * float64(x)
	}
	if len(all) != idx.Dim() || math.Abs(n2-1) > 1e-4 {
		t.Fatalf("Centroid len=%d norm=%f", len(all), n2)
	}

	// CandidateRows: непуст, уважает exclude/forbidden и цели переходов
	row0, _ := idx.RowOf(ids[0])
	rowTarget, _ := idx.RowOf(ids[50])
	cands := idx.CandidateRows(-1, query, map[int64]float64{ids[50]: 3},
		map[int64]bool{ids[0]: true}, map[int]bool{row0: true}, rand.New(rand.NewSource(1)))
	if len(cands) == 0 {
		t.Fatal("CandidateRows returned nothing")
	}
	for _, r := range cands {
		if r == row0 {
			t.Fatal("forbidden row leaked into candidates")
		}
	}
	found := false
	for _, r := range cands {
		if r == rowTarget {
			found = true
		}
	}
	if !found {
		t.Fatal("transition target missing from candidates")
	}

	// центроиды артистов/альбомов: число групп = числу нормформ в мете
	ac := idx.ArtistCentroids()
	if len(ac) == 0 {
		t.Fatal("ArtistCentroids empty")
	}
	if len(ac) != 7 { // i%7 → 7 артистов
		t.Fatalf("ArtistCentroids groups=%d want 7", len(ac))
	}
	for _, g := range ac {
		if len(g.Rows) == 0 || len(g.Vector) != idx.Dim() {
			t.Fatalf("bad artist group %+v", g)
		}
	}
	if len(idx.AlbumCentroids()) != 13 { // i%13 → 13 альбомов
		t.Fatalf("AlbumCentroids groups=%d want 13", len(idx.AlbumCentroids()))
	}
}

// TestPGIndexEmptyOnFreshDB: без активной модели индекс пуст, запросы — no-op.
func TestPGIndexEmptyOnFreshDB(t *testing.T) {
	dsn := pgtest.Open(t)
	store, err := db.OpenPG(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	idx := NewPG(config.Config{}, store.DB)
	if err := idx.Load(nil); err != nil {
		t.Fatal(err)
	}
	if idx.Size() != 0 || idx.TopK([]float32{1}, 5, nil) != nil || idx.Centroid() != nil {
		t.Fatal("empty index must be inert")
	}
}
