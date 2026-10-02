package index

// Общий каркас векторного индекса поверх PostgreSQL: типы строк каталога,
// группировка клонов/артистов/альбомов и конвертация векторов.
// Поисковые методы (SQL по pgvector) — в pg.go.

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
)

// Meta — метаданные строки индекса (без вектора: векторы остаются в pgvector).
type Meta struct {
	ID          int64
	Path        string
	Title       string
	Artist      string
	Album       string
	Duration    float64
	FileMD5     string
	CreatedAt   time.Time
	ArtworkPath string
	ClusterID   int
	Shown       int
	SkipEarly   int
	Completed   int
}

// ScoredRow — строка индекса с косинусной близостью (результат TopK).
type ScoredRow struct {
	Row   int
	Score float32
}

// GroupCentroid — L2-нормированная центроида группы (артист или артист+альбом)
// со списком строк группы.
type GroupCentroid struct {
	Artist string
	Album  string
	Rows   []int
	Vector []float32
}

// SongKey normalizes artist|title for clone grouping.
func SongKey(artist, title string) string {
	a := strings.ToLower(strings.TrimSpace(artist))
	t := strings.ToLower(strings.TrimSpace(title))
	if a == "" || t == "" {
		return ""
	}
	return a + "|" + t
}

// catalog — метаданные строк индекса без векторов; PGIndex строит её
// на Load из строк, которые LoadReadyTracks отдаёт без векторов.
type catalog struct {
	ids  []int64
	meta []Meta
	n    int
	// Clone-поиски, пересобираемые на Load — excludeTrack идёт по картам, не полным сканом.
	idRow        map[int64]int
	md5ToIDs     map[string][]int64
	songKeyToIDs map[string][]int64
	artistRows   map[string][]int
	albumRows    map[string][]int
	artistAlbums map[string][]int
	artistNames  map[string]string
	albumNames   map[string][2]string
}

func buildCatalog(rows []db.TrackRow, dim int) (*catalog, error) {
	n := len(rows)
	cat := &catalog{
		ids:          make([]int64, n),
		meta:         make([]Meta, n),
		n:            n,
		idRow:        make(map[int64]int, n),
		md5ToIDs:     make(map[string][]int64),
		songKeyToIDs: make(map[string][]int64),
		artistRows:   make(map[string][]int),
		albumRows:    make(map[string][]int),
		artistAlbums: make(map[string][]int),
		artistNames:  make(map[string]string),
		albumNames:   make(map[string][2]string),
	}
	for i, r := range rows {
		d := r.Dim
		if d <= 0 {
			d = len(r.Embedding) / 4
		}
		if d != dim {
			return nil, fmt.Errorf("inconsistent embedding dim: %d vs %d", d, dim)
		}
		created, _ := time.Parse(time.RFC3339Nano, r.CreatedAt)
		if created.IsZero() {
			created, _ = time.Parse(time.RFC3339, r.CreatedAt)
		}
		cat.ids[i] = r.ID
		cat.meta[i] = Meta{
			ID: r.ID, Path: r.Path, Title: r.Title, Artist: r.Artist, Album: r.Album,
			Duration: r.Duration, FileMD5: r.FileMD5, CreatedAt: created,
			ArtworkPath: r.ArtworkPath, ClusterID: r.ClusterID,
			Shown: r.Shown, SkipEarly: r.SkipEarly, Completed: r.Completed,
		}
		cat.idRow[r.ID] = i
		if r.FileMD5 != "" {
			cat.md5ToIDs[r.FileMD5] = append(cat.md5ToIDs[r.FileMD5], r.ID)
		}
		if key := SongKey(r.Artist, r.Title); key != "" {
			cat.songKeyToIDs[key] = append(cat.songKeyToIDs[key], r.ID)
		}
		artistKey := normName(r.Artist)
		albumKey := normName(r.Album)
		if artistKey != "" {
			cat.artistRows[artistKey] = append(cat.artistRows[artistKey], i)
			if _, ok := cat.artistNames[artistKey]; !ok {
				cat.artistNames[artistKey] = strings.TrimSpace(r.Artist)
			}
		}
		if albumKey != "" {
			cat.albumRows[albumKey] = append(cat.albumRows[albumKey], i)
			key := artistKey + "\x00" + albumKey
			cat.artistAlbums[key] = append(cat.artistAlbums[key], i)
			if _, ok := cat.albumNames[key]; !ok {
				cat.albumNames[key] = [2]string{strings.TrimSpace(r.Artist), strings.TrimSpace(r.Album)}
			}
		}
	}
	return cat, nil
}

func normName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		return unicode.ToLower(r)
	}, s)
}

// newBoostValue — расчёт буста новых треков (PGIndex.NewBoost).
func newBoostValue(cfg config.Config, m Meta, now time.Time) float32 {
	if m.SkipEarly >= 3 {
		return 0
	}
	if m.CreatedAt.IsZero() {
		return 0
	}
	ageDays := now.Sub(m.CreatedAt).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	if ageDays > float64(cfg.NewTrackDays) {
		return 0
	}
	tau := cfg.NewBoostTauDays
	if tau < 1 {
		tau = 14
	}
	gamma := cfg.NewBoostGamma
	if gamma < 1 {
		gamma = 5
	}
	boost := cfg.NewBoostBeta * math.Exp(-ageDays/tau) * math.Exp(-float64(m.Shown)/gamma)
	return float32(boost)
}

func BytesToFloat32(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(b[i*4 : (i+1)*4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

func Float32Bytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:(i+1)*4], math.Float32bits(f))
	}
	return b
}

func Normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	s = math.Sqrt(s)
	if s < 1e-12 {
		return
	}
	inv := float32(1 / s)
	for i := range v {
		v[i] *= inv
	}
}

func Quantile(vals []float32, q float64) float32 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]float32(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	pos := q * float64(len(cp)-1)
	i := int(pos)
	if i >= len(cp)-1 {
		return cp[len(cp)-1]
	}
	frac := float32(pos - float64(i))
	return cp[i]*(1-frac) + cp[i+1]*frac
}
