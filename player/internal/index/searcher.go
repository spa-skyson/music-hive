package index

import (
	"math/rand"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/db"
)

// Searcher — контракт векторного индекса; единственная реализация —
// PGIndex (pgvector, F1.3). Верхние слои (api/playback/queue/recommend/app)
// работают только через него.
type Searcher interface {
	// Load строит снапшот метаданных из строк (векторы остаются в pgvector).
	Load(rows []db.TrackRow) error

	Size() int
	Dim() int
	RowOf(id int64) (int, bool)
	MetaAt(row int) Meta
	Vector(row int) []float32

	Centroid() []float32
	CentroidOf(rows []int) []float32
	RowsForArtist(artist string) []int
	RowsForAlbum(artist, album string) []int
	ArtistCentroids() []GroupCentroid
	AlbumCentroids() []GroupCentroid

	CloneIDs(trackID int64) []int64
	NewBoost(row int, now time.Time) float32

	// TopK — ближайшие к vec строки по косинусной близости, без исключённых.
	TopK(vec []float32, limit int, exclude map[int64]bool) []ScoredRow
	// SimsTo — близость vec ко всем строкам (индексируется номером строки).
	SimsTo(vec []float32) []float32
	// SimsFor — близость vec к заданным строкам (полный слайс, вне rows — 0).
	SimsFor(vec []float32, rows []int) []float32
	// CandidateRows — шортлист строк для сборки очереди на больших N:
	// кластер текущего трека, цели переходов и ANN-топ по вкусу одним SQL.
	CandidateRows(curCluster int, taste []float32, transitions map[int64]float64,
		exclude map[int64]bool, forbidden map[int]bool, rng *rand.Rand) []int

	BumpShownLocal(id int64)
	BumpSkipEarlyLocal(id int64)
	BumpCompletedLocal(id int64)
}

var _ Searcher = (*PGIndex)(nil)
