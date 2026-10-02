package db_test

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// Живые PG-тесты: MUSIC_HIVE_TEST_DATABASE_URL, иначе skip (паттерн F1.1).
// Каждый тест получает собственную базу (см. pgtest.Open).

func openPG(t *testing.T) *db.PGStore {
	t.Helper()
	store, err := db.OpenPG(pgtest.Open(t))
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// f32bytes/f32from — little-endian float32-байты (формат BLOB-векторов).
func f32bytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		bits := math.Float32bits(f)
		b[4*i], b[4*i+1], b[4*i+2], b[4*i+3] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
	}
	return b
}

func f32from(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(uint32(b[4*i]) | uint32(b[4*i+1])<<8 | uint32(b[4*i+2])<<16 | uint32(b[4*i+3])<<24)
	}
	return out
}

func TestPGStoreCatalogAndFavorites(t *testing.T) {
	store := openPG(t)
	owner := store.UserID()
	for i := 1; i <= 3; i++ {
		pgtest.InsertTrack(t, store.DB, filepath.Join("/music", fmt.Sprintf("t%d.flac", i)),
			"Artist One", "Album X", fmt.Sprintf("Track %d", i))
	}
	catalog, err := store.ListCatalogTracks()
	if err != nil || len(catalog) != 3 {
		t.Fatalf("catalog=%d err=%v, want 3 tracks", len(catalog), err)
	}
	if catalog[0].Status != "pending" {
		t.Fatalf("catalog status without model = %q, want pending", catalog[0].Status)
	}
	path, err := store.TrackPath(catalog[0].ID)
	if err != nil || path == "" {
		t.Fatalf("TrackPath: %q %v", path, err)
	}
	if _, err := store.TrackPath(999999); err != nil {
		t.Fatalf("TrackPath missing should be empty, got %v", err)
	}

	// favorites: треки
	id1, id2 := catalog[0].ID, catalog[1].ID
	if err := store.FavoritesAdd(owner, id1); err != nil {
		t.Fatal(err)
	}
	if err := store.FavoritesAdd(owner, id1); err != nil { // повторный — upsert
		t.Fatal(err)
	}
	if err := store.FavoritesAdd(owner, id2); err != nil {
		t.Fatal(err)
	}
	if !store.FavoritesHas(owner, id1) || store.FavoritesCount(owner) != 2 {
		t.Fatalf("favorites has/count mismatch: %v %d", store.FavoritesHas(owner, id1), store.FavoritesCount(owner))
	}
	favs, err := store.FavoritesList(owner)
	if err != nil || len(favs) != 2 {
		t.Fatalf("favorites list=%d err=%v", len(favs), err)
	}
	if err := store.FavoritesRemove(owner, id1); err != nil {
		t.Fatal(err)
	}
	if store.FavoritesHas(owner, id1) || store.FavoritesCount(owner) != 1 {
		t.Fatal("favorites remove failed")
	}

	// favorite artists / albums (upsert сущностей по name_norm)
	if err := store.FavArtistAdd(owner, "  Artist   ONE "); err != nil { // нормформа == "artist one"
		t.Fatal(err)
	}
	if !store.FavArtistHas(owner, "Artist One") || store.FavArtistCount(owner) != 1 {
		t.Fatalf("fav artist has/count: %v %d", store.FavArtistHas(owner, "Artist One"), store.FavArtistCount(owner))
	}
	artists, err := store.FavArtistsList(owner)
	if err != nil || len(artists) != 1 || artists[0].Artist == "" {
		t.Fatalf("fav artists list: %+v err=%v", artists, err)
	}
	if err := store.FavAlbumAdd(owner, "Artist One", "Album X"); err != nil {
		t.Fatal(err)
	}
	if !store.FavAlbumHas(owner, "artist one", "ALBUM   x") || store.FavAlbumCount(owner) != 1 {
		t.Fatalf("fav album has/count mismatch")
	}
	albums, err := store.FavAlbumsList(owner)
	if err != nil || len(albums) != 1 || albums[0].Album != "Album X" {
		t.Fatalf("fav albums list: %+v err=%v", albums, err)
	}
	if err := store.FavAlbumRemove(owner, "Artist One", "Album X"); err != nil {
		t.Fatal(err)
	}
	if store.FavAlbumHas(owner, "Artist One", "Album X") {
		t.Fatal("fav album remove failed")
	}
	if err := store.FavArtistRemove(owner, "Artist One"); err != nil {
		t.Fatal(err)
	}
	if store.FavArtistHas(owner, "Artist One") {
		t.Fatal("fav artist remove failed")
	}

	// listen later
	if err := store.LaterAdd(owner, id1); err != nil {
		t.Fatal(err)
	}
	if store.LaterCount(owner) != 1 {
		t.Fatalf("later count=%d want 1", store.LaterCount(owner))
	}
	later, err := store.LaterList(owner)
	if err != nil || len(later) != 1 || later[0].TrackID != id1 {
		t.Fatalf("later list: %+v err=%v", later, err)
	}
	if err := store.LaterRemove(owner, id1); err != nil || store.LaterCount(owner) != 0 {
		t.Fatalf("later remove: %v count=%d", err, store.LaterCount(owner))
	}
}

func TestPGStoreHistoryJobsMetrics(t *testing.T) {
	store := openPG(t)
	owner := store.UserID()
	id1 := pgtest.InsertTrack(t, store.DB, "/music/h1.flac", "A", "B", "H1")
	id2 := pgtest.InsertTrack(t, store.DB, "/music/h2.flac", "A", "B", "H2")

	// история + недавние
	if _, err := store.InsertListen(owner, id1, "track_end", "player", "s1", "completed",
		nil, ptrFloat(200), ptrFloat(195)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := store.InsertListen(owner, id2, "like", "player", "s1", "",
		nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	recent, err := store.RecentTrackIDs(owner, 24, 10)
	if err != nil || len(recent) != 2 || recent[0] != id2 {
		t.Fatalf("recent=%v err=%v, want newest-first [%d %d]", recent, err, id2, id1)
	}
	pos, neg, err := store.ListenSignalCounts(owner)
	if err != nil || pos != 2 {
		t.Fatalf("signals pos=%d neg=%d err=%v, want 2/0", pos, neg, err)
	}

	// переходы
	if err := store.BumpTransition(id1, id2, 1.5); err != nil {
		t.Fatal(err)
	}
	if err := store.BumpTransition(id1, id2, 0.5); err != nil {
		t.Fatal(err)
	}
	graph, err := store.LoadTransitionGraph()
	if err != nil || graph[id1][id2] != 2.0 {
		t.Fatalf("transitions: %v err=%v", graph[id1][id2], err)
	}

	// статистика показов
	if err := store.BumpRecStats(owner, id1, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.BumpRecStats(owner, id1, 1, 1, 1); err != nil {
		t.Fatal(err)
	}

	// импрешены + метрики
	if err := store.InsertRecommendationImpressions(owner, []db.RecommendationImpression{
		{SessionID: "s1", TrackID: id1, Position: 0, Explore: true, Score: 0.5},
		{SessionID: "s1", TrackID: id2, Position: 1, Explore: false, Score: 0.9},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := store.WeeklyMetrics(owner)
	if err != nil {
		t.Fatal(err)
	}
	if m.Listens7d != 2 || m.ExploreShown != 1 || m.ExploitShown != 1 {
		t.Fatalf("weekly metrics: %+v", m)
	}
	top, err := store.TopArtists(owner, 5)
	if err != nil || len(top) != 1 || top[0].Count != 2 {
		t.Fatalf("top artists: %+v err=%v", top, err)
	}

	// джобы: enqueue → done → polling по времени
	jobID, err := store.EnqueueJob("embed", `{"x":1}`)
	if err != nil || jobID == 0 {
		t.Fatalf("enqueue: %d %v", jobID, err)
	}
	got, err := store.GetJob(jobID)
	if err != nil || got == nil || got.Status != "pending" {
		t.Fatalf("get job: %+v err=%v", got, err)
	}
	// jsonb нормализует представление (пробелы) — сравниваем распарсенно
	var payloadGot, payloadWant any
	if err := json.Unmarshal([]byte(got.Payload), &payloadGot); err != nil {
		t.Fatalf("job payload not json: %q", got.Payload)
	}
	if err := json.Unmarshal([]byte(`{"x":1}`), &payloadWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payloadGot, payloadWant) {
		t.Fatalf("job payload: %q", got.Payload)
	}
	before := time.Now().UTC().Add(-time.Second)
	if _, err := store.DB.Exec(`UPDATE jobs SET status='done', result_json='"ok"', updated_at=$1 WHERE id=$2`,
		before.Add(2*time.Second), jobID); err != nil {
		t.Fatal(err)
	}
	done, err := store.ListDoneJobsAfter(before.Format(time.RFC3339Nano), 10)
	if err != nil || len(done) != 1 || done[0].ID != jobID || done[0].Result != `"ok"` {
		t.Fatalf("done jobs: %+v err=%v", done, err)
	}
	// эксклюзивность границы
	done, _ = store.ListDoneJobsAfter(done[0].UpdatedAt, 10)
	if len(done) != 0 {
		t.Fatalf("ListDoneJobsAfter must be exclusive, got %d", len(done))
	}
	byStatus, err := store.ListJobs("done", 10)
	if err != nil || len(byStatus) != 1 {
		t.Fatalf("list jobs by status: %d err=%v", len(byStatus), err)
	}
}

func TestPGStoreSessionsRadioLyricsProfilePlaylists(t *testing.T) {
	store := openPG(t)
	owner := store.UserID()
	id1 := pgtest.InsertTrack(t, store.DB, "/music/p1.flac", "A", "B", "P1")

	// play-сессии
	row := db.PlaySessionRow{ID: "sess-1", UserID: owner, Mode: "radio", CurrentID: id1, QueueJSON: "[1,2]",
		DailyPos: 3, PlaylistName: "Daily", PlaylistKind: "daily",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := store.UpsertPlaySession(row); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.LoadPlaySession(owner, "sess-1")
	if err != nil || !ok || loaded.Mode != "radio" || loaded.DailyPos != 3 {
		t.Fatalf("play session: %+v ok=%v err=%v", loaded, ok, err)
	}
	// jsonb нормализует представление — сравниваем распарсенно
	var queueGot, queueWant any
	if err := json.Unmarshal([]byte(loaded.QueueJSON), &queueGot); err != nil {
		t.Fatalf("queue not json: %q", loaded.QueueJSON)
	}
	if err := json.Unmarshal([]byte(row.QueueJSON), &queueWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(queueGot, queueWant) {
		t.Fatalf("queue json: %q want %q", loaded.QueueJSON, row.QueueJSON)
	}
	if n, _ := store.CountPlaySessions(); n != 1 {
		t.Fatalf("play sessions count=%d", n)
	}
	ids, err := store.OldestPlaySessionIDs(5)
	if err != nil || len(ids) != 1 || ids[0] != "sess-1" {
		t.Fatalf("oldest sessions: %v err=%v", ids, err)
	}
	if err := store.DeleteStalePlaySessions(time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountPlaySessions(); n != 1 {
		t.Fatalf("stale delete removed live session: %d", n)
	}
	if err := store.DeletePlaySession("sess-1"); err != nil {
		t.Fatal(err)
	}

	// radio shares
	share, err := store.CreateRadioShare(owner, "tok123", "kitchen")
	if err != nil || !share.Active {
		t.Fatalf("create share: %+v err=%v", share, err)
	}
	active, ok, err := store.GetActiveRadioShare("tok123")
	if err != nil || !ok || active.Name != "kitchen" {
		t.Fatalf("get share: %+v ok=%v err=%v", active, ok, err)
	}
	if err := store.TouchRadioShareListen("tok123"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListRadioShares(owner, false)
	if err != nil || len(listed) != 1 || listed[0].ListenCount != 1 || listed[0].LastListenAt == nil {
		t.Fatalf("list shares: %+v err=%v", listed, err)
	}
	if err := store.RevokeRadioShare(owner, "tok123"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.GetActiveRadioShare("tok123"); ok {
		t.Fatal("revoked share must be inactive")
	}

	// lyrics
	if _, err := store.DB.Exec(`
INSERT INTO lyrics(track_id, plain_lyrics, synced_lyrics, source, source_id, instrumental, status, updated_at)
VALUES ($1,'la-la','','test','',TRUE,'ready',now())`, id1); err != nil {
		t.Fatal(err)
	}
	ly, ok, err := store.GetLyrics(id1)
	if err != nil || !ok || ly.PlainLyrics != "la-la" || !ly.Instrumental {
		t.Fatalf("lyrics: %+v ok=%v err=%v", ly, ok, err)
	}

	// профиль вкуса: без активной модели — no-op, с моделью — round-trip
	if err := store.SaveProfile(owner, "global", []byte{1, 0, 0, 0}); err != nil {
		t.Fatalf("save profile without model should be no-op: %v", err)
	}
	pgtest.ActivateModel(t, store.DB, "test:profile", 2, false)
	vec := []float32{0.6, 0.8}
	if err := store.SaveProfile(owner, "global", f32bytes(vec)); err != nil {
		t.Fatal(err)
	}
	blob, err := store.LatestProfile(owner, "global")
	if err != nil || len(blob) != 8 {
		t.Fatalf("latest profile: %v err=%v", blob, err)
	}
	back := f32from(blob)
	if len(back) != 2 || back[0] != 0.6 || back[1] != 0.8 {
		t.Fatalf("profile round-trip: %v", back)
	}
	if err := store.PruneProfiles(owner, "global", 1); err != nil {
		t.Fatal(err)
	}

	// плейлисты (их пишет worker; player читает)
	var plID int64
	if err := store.DB.QueryRow(`
INSERT INTO playlists(owner_user_id, kind, name) VALUES ($1,'daily','Daily 1') RETURNING id`,
		store.UserID()).Scan(&plID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`
INSERT INTO playlist_tracks(playlist_id, position, track_id) VALUES ($1,0,$2),($1,1,$2)`,
		plID, id1); err != nil {
		t.Fatal(err)
	}
	pid, name, n, err := store.PlaylistMeta(owner, "daily")
	if err != nil || pid != plID || name != "Daily 1" || n != 2 {
		t.Fatalf("playlist meta: %d %q %d err=%v", pid, name, n, err)
	}
	latest, err := store.LatestPlaylist(owner, "daily")
	if err != nil || latest == nil || len(latest.Tracks) != 2 {
		t.Fatalf("latest playlist: %+v err=%v", latest, err)
	}
	if _, err := store.DB.Exec(`
INSERT INTO discover_tips(user_id, kind, artist, score, track_ids_json)
VALUES ($1,'artist','A',0.9,'[1,2]')`, store.UserID()); err != nil {
		t.Fatal(err)
	}
	tips, err := store.ListDiscoverTips(owner, "artist", 5)
	if err != nil || len(tips) != 1 || len(tips[0].TrackIDs) != 2 {
		t.Fatalf("discover tips: %+v err=%v", tips, err)
	}
}

func ptrFloat(f float64) *float64 { return &f }

// TestPGLatestPlaylistSelectsNewestAndPreservesPositions мигрирован из
// db_test.go: TestPGStoreSessionsRadioLyricsProfilePlaylists покрывает
// LatestPlaylist/PlaylistMeta лишь с одним плейлистом и позициями по
// порядку (0,1) — здесь же проверяется выбор САМОГО НОВОГО плейлиста
// среди нескольких того же kind и сохранение непоследовательных позиций.
func TestPGLatestPlaylistSelectsNewestAndPreservesPositions(t *testing.T) {
	store := openPG(t)
	owner := store.UserID()
	id1 := pgtest.InsertTrack(t, store.DB, "/track-1.flac", "Artist", "", "Track 1")
	id2 := pgtest.InsertTrack(t, store.DB, "/track-2.flac", "Artist", "", "Track 2")
	id3 := pgtest.InsertTrack(t, store.DB, "/track-3.flac", "Artist", "", "Track 3")

	var oldID, newID, otherID int64
	if err := store.DB.QueryRow(`
INSERT INTO playlists(owner_user_id, kind, name, created_at) VALUES ($1,'daily','Old','2026-08-14T00:00:00Z') RETURNING id`,
		owner).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`
INSERT INTO playlists(owner_user_id, kind, name, created_at) VALUES ($1,'daily','New','2026-08-15T00:00:00Z') RETURNING id`,
		owner).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.QueryRow(`
INSERT INTO playlists(owner_user_id, kind, name, created_at) VALUES ($1,'weekly','Other kind','2026-08-16T00:00:00Z') RETURNING id`,
		owner).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	_ = otherID
	if _, err := store.DB.Exec(`
INSERT INTO playlist_tracks(playlist_id, position, track_id, explanation) VALUES
  ($1,0,$3,'old'), ($2,7,$4,'second'), ($2,3,$5,'first')`,
		oldID, newID, id1, id2, id3); err != nil {
		t.Fatal(err)
	}

	playlist, err := store.LatestPlaylist(owner, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if playlist == nil || playlist.ID != newID || playlist.Name != "New" {
		t.Fatalf("got playlist %#v, want newest daily playlist %d", playlist, newID)
	}
	if len(playlist.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(playlist.Tracks))
	}
	if got := []int{playlist.Tracks[0].Position, playlist.Tracks[1].Position}; got[0] != 3 || got[1] != 7 {
		t.Fatalf("positions = %v, want [3 7]", got)
	}
	if playlist.Tracks[0].TrackID != id3 || playlist.Tracks[1].TrackID != id2 {
		t.Fatalf("track order = [%d %d], want [%d %d]", playlist.Tracks[0].TrackID, playlist.Tracks[1].TrackID, id3, id2)
	}
}

// TestPGRecommendationImpressionsJoinOutcomes мигрирован из db_test.go:
// TestPGStoreHistoryJobsMetrics проверяет только Listens7d/ExploreShown/
// ExploitShown — здесь дополнительно покрыты skip/complete-исходы и
// производные rate-метрики.
func TestPGRecommendationImpressionsJoinOutcomes(t *testing.T) {
	store := openPG(t)
	owner := store.UserID()
	id1 := pgtest.InsertTrack(t, store.DB, "/track-1.flac", "A", "B", "Track 1")
	id2 := pgtest.InsertTrack(t, store.DB, "/track-2.flac", "A", "B", "Track 2")

	if err := store.InsertRecommendationImpressions(owner, []db.RecommendationImpression{
		{SessionID: "s1", TrackID: id1, Position: 0, Explore: true, Score: 0.2},
		{SessionID: "s1", TrackID: id2, Position: 1, Explore: false, Score: 0.8},
	}); err != nil {
		t.Fatal(err)
	}
	duration := 100.0
	skipped := 10.0
	completed := 95.0
	if _, err := store.InsertListen(owner, id1, "track_end", "test", "s1", "skipped", nil, &duration, &skipped); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertListen(owner, id2, "track_end", "test", "s1", "completed", nil, &duration, &completed); err != nil {
		t.Fatal(err)
	}
	metrics, err := store.WeeklyMetrics(owner)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.ExploreShown != 1 || metrics.ExploitShown != 1 {
		t.Fatalf("shown explore/exploit=%d/%d, want 1/1", metrics.ExploreShown, metrics.ExploitShown)
	}
	if metrics.ExploreSkips != 1 || metrics.ExploitComplete != 1 {
		t.Fatalf("outcomes explore skips=%d exploit completes=%d, want 1/1",
			metrics.ExploreSkips, metrics.ExploitComplete)
	}
	if metrics.ExploreSkipRate != 1 || metrics.ExploitCompRate != 1 {
		t.Fatalf("rates explore skip=%f exploit complete=%f, want 1/1",
			metrics.ExploreSkipRate, metrics.ExploitCompRate)
	}
}
