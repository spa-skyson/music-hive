package index

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
)

// PGIndex — единственная реализация Searcher: pgvector, полной матрицы
// в памяти нет, каждый поисковый метод — SQL по таблице emb_<active>
// (HNSW-индекс, design §6.3). Load строит только снапшот метаданных из
// строк, которые LoadReadyTracks PGStore отдаёт без векторов.
type PGIndex struct {
	mu  sync.RWMutex
	cfg config.Config
	sql *sql.DB

	cat *catalog
	n   int
	dim int
	emb string // имя таблицы векторов активной модели
}

func NewPG(cfg config.Config, database *sql.DB) *PGIndex {
	return &PGIndex{cfg: cfg, sql: database}
}

// Load строит метаданные из rows (Embedding в PG-режиме всегда nil) и
// переразрешает активную модель по реестру embedding_models.
func (idx *PGIndex) Load(rows []db.TrackRow) error {
	if len(rows) == 0 {
		// Пустой каталог мог стать пустым и из-за отсутствия активной модели —
		// реестр всё равно перечитываем (модель могли активировать).
		emb, dim, err := idx.resolveActive()
		if err != nil {
			return err
		}
		idx.mu.Lock()
		idx.cat, idx.n, idx.dim, idx.emb = nil, 0, dim, emb
		idx.mu.Unlock()
		return nil
	}
	dim := rows[0].Dim
	if dim <= 0 {
		return fmt.Errorf("pg index: track rows carry no embedding dim (model registry empty?)")
	}
	cat, err := buildCatalog(rows, dim)
	if err != nil {
		return err
	}
	emb, _, err := idx.resolveActive()
	if err != nil {
		return err
	}
	idx.mu.Lock()
	idx.cat, idx.n, idx.dim, idx.emb = cat, len(rows), dim, emb
	idx.mu.Unlock()
	return nil
}

// resolveActive возвращает (emb-таблица, dim) активной модели; таблица
// может ещё не существовать (job model_activate не отработал) — тогда
// векторные запросы невозможны и индекс ведёт себя как пустой.
func (idx *PGIndex) resolveActive() (string, int, error) {
	var key string
	var dim int
	err := idx.sql.QueryRow(
		`SELECT model_key, dim FROM embedding_models WHERE is_active`).Scan(&key, &dim)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	table := db.EmbTableName(key)
	var reg *string
	if err := idx.sql.QueryRow(`SELECT to_regclass($1)`, "public."+table).Scan(&reg); err != nil {
		return "", 0, err
	}
	if reg == nil {
		log.Printf("pg index: model %s active but table %s missing (waiting for model_activate)", key, table)
		return "", dim, nil
	}
	return table, dim, nil
}

func (idx *PGIndex) ready() (string, int, int, map[int64]int, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.cat == nil || idx.emb == "" || idx.n == 0 {
		return "", 0, 0, nil, false
	}
	return idx.emb, idx.dim, idx.n, idx.cat.idRow, true
}

func (idx *PGIndex) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.n
}

func (idx *PGIndex) Dim() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.dim
}

func (idx *PGIndex) RowOf(id int64) (int, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.cat == nil {
		return 0, false
	}
	r, ok := idx.cat.idRow[id]
	return r, ok
}

func (idx *PGIndex) MetaAt(row int) Meta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.cat == nil || row < 0 || row >= idx.n {
		return Meta{}
	}
	return idx.cat.meta[row]
}

func (idx *PGIndex) CloneIDs(trackID int64) []int64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if idx.cat == nil {
		return []int64{trackID}
	}
	row, ok := idx.cat.idRow[trackID]
	if !ok {
		return []int64{trackID}
	}
	m := idx.cat.meta[row]
	seen := map[int64]bool{trackID: true}
	out := []int64{trackID}
	add := func(ids []int64) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	if m.FileMD5 != "" {
		add(idx.cat.md5ToIDs[m.FileMD5])
	}
	if key := SongKey(m.Artist, m.Title); key != "" {
		add(idx.cat.songKeyToIDs[key])
	}
	return out
}

func (idx *PGIndex) RowsForArtist(artist string) []int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	want := normName(artist)
	if want == "" || idx.cat == nil {
		return nil
	}
	return append([]int(nil), idx.cat.artistRows[want]...)
}

func (idx *PGIndex) RowsForAlbum(artist, album string) []int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	wantA := normName(artist)
	wantAl := normName(album)
	if wantAl == "" || idx.cat == nil {
		return nil
	}
	if wantA == "" {
		return append([]int(nil), idx.cat.albumRows[wantAl]...)
	}
	return append([]int(nil), idx.cat.artistAlbums[wantA+"\x00"+wantAl]...)
}

func (idx *PGIndex) NewBoost(row int, now time.Time) float32 {
	return newBoostValue(idx.cfg, idx.MetaAt(row), now)
}

func (idx *PGIndex) BumpShownLocal(id int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.cat == nil {
		return
	}
	if r, ok := idx.cat.idRow[id]; ok {
		idx.cat.meta[r].Shown++
	}
}

func (idx *PGIndex) BumpSkipEarlyLocal(id int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.cat == nil {
		return
	}
	if r, ok := idx.cat.idRow[id]; ok {
		idx.cat.meta[r].SkipEarly++
	}
}

func (idx *PGIndex) BumpCompletedLocal(id int64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.cat == nil {
		return
	}
	if r, ok := idx.cat.idRow[id]; ok {
		idx.cat.meta[r].Completed++
	}
}

// Vector возвращает вектор строки (один SQL-запрос; вызывается редко —
// события воспроизведения и точечные рекомендации).
func (idx *PGIndex) Vector(row int) []float32 {
	emb, dim, n, _, ok := idx.ready()
	if !ok || row < 0 || row >= n {
		return nil
	}
	id := idx.MetaAt(row).ID
	var vec pgvector.Vector
	err := idx.sql.QueryRow(
		`SELECT embedding FROM `+emb+` WHERE track_id = $1`, id).Scan(&vec)
	if err != nil {
		return nil
	}
	v := vec.Slice()
	if len(v) < dim {
		return nil
	}
	return v[:dim]
}

// TopK — ANN-поиск: ORDER BY embedding <=> $1 LIMIT k (HNSW).
// Косинусная близость 1 - distance,
// исключённые track_id отфильтрованы (pgvector ≥0.8 — iterative scan).
func (idx *PGIndex) TopK(vec []float32, limit int, exclude map[int64]bool) []ScoredRow {
	emb, dim, _, _, ok := idx.ready()
	if !ok || limit <= 0 || len(vec) != dim {
		return nil
	}
	excluded := make([]int64, 0, len(exclude))
	for id := range exclude {
		excluded = append(excluded, id)
	}
	rows, err := idx.sql.Query(`
SELECT e.track_id, 1 - (e.embedding <=> $1::vector) AS sim
FROM `+emb+` e
JOIN tracks t ON t.id = e.track_id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND e.status = 'ready'
  AND e.track_id <> ALL($2::bigint[])
ORDER BY e.embedding <=> $1::vector
LIMIT $3`, pgvector.NewVector(vec), db.Int64Array(excluded), limit)
	if err != nil {
		log.Printf("pg index TopK: %v", err)
		return nil
	}
	defer rows.Close()
	var out []ScoredRow
	for rows.Next() {
		var id int64
		var sim float32
		if err := rows.Scan(&id, &sim); err != nil {
			log.Printf("pg index TopK scan: %v", err)
			return nil
		}
		idx.mu.RLock()
		row, ok := idx.cat.idRow[id]
		idx.mu.RUnlock()
		if !ok {
			continue
		}
		out = append(out, ScoredRow{Row: row, Score: sim})
	}
	return out
}

// SimsTo — точные близости ко всем строкам (одним seq scan по emb-таблице);
// в памяти только слайс результата на N float32.
func (idx *PGIndex) SimsTo(vec []float32) []float32 {
	emb, dim, _, _, ok := idx.ready()
	n := idx.Size()
	if !ok || len(vec) != dim {
		return make([]float32, n)
	}
	return idx.simsQuery(emb, vec, n, nil)
}

// SimsFor — близости к подмножеству строк (pooled-путь сборки очереди).
func (idx *PGIndex) SimsFor(vec []float32, rows []int) []float32 {
	emb, dim, n, _, ok := idx.ready()
	out := make([]float32, n)
	if !ok || len(vec) != dim {
		return out
	}
	if len(rows) == 0 {
		return out
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r >= 0 && r < n {
			ids = append(ids, idx.MetaAt(r).ID)
		}
	}
	return idx.simsQuery(emb, vec, n, ids)
}

// simsQuery возвращает слайс длины n: sims для запрошенных track_id
// (nil ids = все строки индекса), остальные — нули.
func (idx *PGIndex) simsQuery(emb string, vec []float32, n int, ids []int64) []float32 {
	idx.mu.RLock()
	idRow := idx.cat.idRow
	idx.mu.RUnlock()
	out := make([]float32, n)
	filter := ""
	args := []any{pgvector.NewVector(vec)}
	if ids != nil {
		filter = ` AND e.track_id = ANY($2::bigint[])`
		args = append(args, db.Int64Array(ids))
	}
	rows, err := idx.sql.Query(`
SELECT e.track_id, 1 - (e.embedding <=> $1::vector) AS sim
FROM `+emb+` e
JOIN tracks t ON t.id = e.track_id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND e.status = 'ready'`+filter, args...)
	if err != nil {
		log.Printf("pg index sims: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var sim float32
		if err := rows.Scan(&id, &sim); err != nil {
			log.Printf("pg index sims scan: %v", err)
			return out
		}
		if row, ok := idRow[id]; ok {
			out[row] = sim
		}
	}
	return out
}

// CandidateRows — шортлист строк для сборки очереди на больших N: кластер
// текущего трека, цели переходов и ANN-топ по вкусу одним SQL
// (design §6.3 предписывает именно ANN top-K).
// ponytail: ORDER BY random() — seq scan; на сотнях тысяч строк заменить
// на TABLESAMPLE.
func (idx *PGIndex) CandidateRows(
	curCluster int, taste []float32, transitions map[int64]float64,
	exclude map[int64]bool, forbidden map[int]bool, rng *rand.Rand,
) []int {
	emb, dim, n, _, ok := idx.ready()
	if !ok || len(taste) != dim {
		return nil
	}
	idx.mu.RLock()
	idRow := idx.cat.idRow
	meta := idx.cat.meta
	excluded := make([]int64, 0, len(exclude)+len(forbidden))
	for id := range exclude {
		excluded = append(excluded, id)
	}
	for row := range forbidden {
		if row >= 0 && row < n {
			excluded = append(excluded, meta[row].ID)
		}
	}
	idx.mu.RUnlock()
	transIDs := make([]int64, 0, len(transitions))
	for toID := range transitions {
		transIDs = append(transIDs, toID)
	}

	q := `
SELECT track_id FROM (
  (SELECT track_id FROM track_audio_features WHERE cluster_id = $1)
  UNION
  (SELECT unnest($2::bigint[]))
  UNION
  (SELECT track_id FROM ` + emb + ` ORDER BY embedding <=> $3::vector LIMIT 1500)
  UNION
  (SELECT track_id FROM ` + emb + ` ORDER BY random() LIMIT 800)
) c
WHERE track_id <> ALL($4::bigint[])`
	cluster := -1
	if curCluster >= 0 {
		cluster = curCluster
	}
	// Кластерная часть необязательна: -1 не матчится ни с одним cluster_id,
	// отдельного ветвления SQL не нужно.
	rows, err := idx.sql.Query(q, cluster, db.Int64Array(transIDs),
		pgvector.NewVector(taste), db.Int64Array(excluded))
	if err != nil {
		log.Printf("pg index candidates: %v", err)
		return nil
	}
	defer rows.Close()
	out := make([]int, 0, 64)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			log.Printf("pg index candidates scan: %v", err)
			return nil
		}
		if row, ok := idRow[id]; ok {
			out = append(out, row)
		}
	}
	return out
}

// Centroid — средний вектор библиотеки (SQL-агрегат, без выгрузки матрицы).
func (idx *PGIndex) Centroid() []float32 {
	emb, _, _, _, ok := idx.ready()
	if !ok {
		return nil
	}
	var vec pgvector.Vector
	err := idx.sql.QueryRow(
		`SELECT avg(e.embedding) FROM ` + emb + ` e
JOIN tracks t ON t.id = e.track_id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND e.status = 'ready'`).Scan(&vec)
	if err != nil {
		log.Printf("pg index centroid: %v", err)
		return nil
	}
	out := vec.Slice()
	Normalize(out)
	return out
}

// CentroidOf — L2-нормированное среднее векторов заданных строк.
func (idx *PGIndex) CentroidOf(rows []int) []float32 {
	emb, dim, n, _, ok := idx.ready()
	if !ok || len(rows) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r >= 0 && r < n {
			ids = append(ids, idx.MetaAt(r).ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var vec pgvector.Vector
	err := idx.sql.QueryRow(
		`SELECT avg(embedding) FROM `+emb+` WHERE track_id = ANY($1::bigint[])`,
		db.Int64Array(ids)).Scan(&vec)
	if err != nil {
		log.Printf("pg index centroidOf: %v", err)
		return nil
	}
	out := vec.Slice()
	if len(out) < dim {
		return nil
	}
	Normalize(out)
	return out
}

// ArtistCentroids — центроиды артистов на лету (SQL group-avg по активной
// модели). emb_*_groups (поддерживает worker, F1.4) здесь не читается:
// ponytail: переключить на_groups, когда worker начнёт их наполнять,
// если group-avg станет узким местом.
func (idx *PGIndex) ArtistCentroids() []GroupCentroid {
	return idx.groupCentroids(false)
}

func (idx *PGIndex) AlbumCentroids() []GroupCentroid {
	return idx.groupCentroids(true)
}

func (idx *PGIndex) groupCentroids(albums bool) []GroupCentroid {
	emb, dim, _, _, ok := idx.ready()
	if !ok {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	selectPrefix := `(array_agg(t.artist ORDER BY t.id))[1] AS display`
	groupExpr := `lower(btrim(t.artist))`
	whereExtra := ``
	if albums {
		selectPrefix = `(array_agg(t.artist ORDER BY t.id))[1] AS display_artist,
       (array_agg(t.album ORDER BY t.id))[1] AS display_album`
		groupExpr = `lower(btrim(t.artist)), lower(btrim(t.album))`
		whereExtra = ` AND btrim(t.album) <> ''`
	}
	rows, err := idx.sql.Query(`
SELECT ` + selectPrefix + `, avg(e.embedding) AS centroid
FROM ` + emb + ` e
JOIN tracks t ON t.id = e.track_id
WHERE t.is_active AND t.is_duplicate_of IS NULL AND e.status = 'ready'` + whereExtra + `
GROUP BY ` + groupExpr)
	if err != nil {
		log.Printf("pg index groups: %v", err)
		return nil
	}
	defer rows.Close()
	var out []GroupCentroid
	for rows.Next() {
		var displayArtist, displayAlbum string
		var vec pgvector.Vector
		var dest []any
		if albums {
			dest = []any{&displayArtist, &displayAlbum, &vec}
		} else {
			dest = []any{&displayArtist, &vec}
			displayAlbum = ""
		}
		if err := rows.Scan(dest...); err != nil {
			log.Printf("pg index groups scan: %v", err)
			return nil
		}
		v := vec.Slice()
		if len(v) < dim {
			continue
		}
		Normalize(v)
		var key string
		if albums {
			key = normName(displayArtist) + "\x00" + normName(displayAlbum)
		} else {
			key = normName(displayArtist)
		}
		var groupRows []int
		if albums {
			groupRows = idx.cat.artistAlbums[key]
		} else {
			groupRows = idx.cat.artistRows[key]
		}
		if len(groupRows) == 0 {
			continue
		}
		out = append(out, GroupCentroid{
			Artist: displayArtist, Album: displayAlbum,
			Rows: append([]int(nil), groupRows...), Vector: v,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Artist != out[j].Artist {
			return out[i].Artist < out[j].Artist
		}
		return out[i].Album < out[j].Album
	})
	return out
}
