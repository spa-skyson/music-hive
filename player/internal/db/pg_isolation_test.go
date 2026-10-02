package db_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/pgtest"
)

// TestPGStoreUserIsolation — ключевой DoD F2.2 (GitLab #20): данные
// пользователя A невидимы и недоступны пользователю B; is_admin ≠ доступ к
// чужим данным; прямые обращения к чужим объектам по id → пусто/не найдено
// (существование не раскрываем).
func TestPGStoreUserIsolation(t *testing.T) {
	store := openPG(t)
	a := store.UserID() // владелец (bootstrap OpenPG)
	b := pgtest.InsertUser(t, store.DB, "bob", "pass-b", false)

	// модель + трек, чтобы писались и профили, и статы
	const dim = 4
	pgtest.ActivateModel(t, store.DB, "test:iso", dim, false)
	id1 := pgtest.InsertTrack(t, store.DB, "/iso/a1.flac", "Artist A", "Album A", "A1")
	id2 := pgtest.InsertTrack(t, store.DB, "/iso/a2.flac", "Artist A", "Album A", "A2")
	pgtest.InsertEmbedding(t, store.DB, "test:iso", id1, []float32{1, 0, 0, 0})
	pgtest.InsertEmbedding(t, store.DB, "test:iso", id2, []float32{0, 1, 0, 0})

	// ---------- A наполняет свои данные ----------
	mustT(t, store.FavoritesAdd(a, id1))
	mustT(t, store.FavoritesAdd(a, id2))
	mustT(t, store.FavArtistAdd(a, "Artist A"))
	mustT(t, store.FavAlbumAdd(a, "Artist A", "Album A"))
	mustT(t, store.LaterAdd(a, id1))
	mustT(t, store.BumpRecStats(a, id1, 1, 0, 0))
	mustT(t, store.InsertRecommendationImpressions(a, []db.RecommendationImpression{{
		SessionID: "sess-iso", TrackID: id1, Position: 0, Score: 1,
		Explore: true, Maturity: "discovering", Mode: "radio",
	}}))
	_, e1 := store.InsertListen(a, id1, "track_end", "player", "sess-iso", "completed",
		nil, ptrFloat(200), ptrFloat(195))
	mustT(t, e1)
	_, e2 := store.InsertListen(a, id1, "like", "player", "sess-iso", "", nil, nil, nil)
	mustT(t, e2)
	mustT(t, store.SaveProfile(a, "global", isoVec(dim)))
	_, e3 := store.CreateRadioShare(a, "tok-a", "radio A")
	mustT(t, e3)
	mustT(t, store.UpsertPlaySession(db.PlaySessionRow{
		ID: "sess-iso-a", UserID: a, Mode: "radio", CurrentID: id1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}))
	// плейлист и подсказки пишет worker — сеем напрямую SQL
	var plID int64
	mustT(t, store.DB.QueryRow(`
INSERT INTO playlists (owner_user_id, kind, name) VALUES ($1, 'daily', 'Daily A') RETURNING id`,
		a).Scan(&plID))
	_, err := store.DB.Exec(
		`INSERT INTO playlist_tracks (playlist_id, position, track_id) VALUES ($1, 0, $2)`, plID, id1)
	mustT(t, err)
	_, err = store.DB.Exec(`
INSERT INTO discover_tips (user_id, kind, artist, album, score, track_ids_json)
VALUES ($1, 'new_album', 'Artist A', 'Album A', 1, $2)`, a, "["+strconv.FormatInt(id1, 10)+"]")
	mustT(t, err)

	// ---------- B видит пустоту ----------
	if list, err := store.FavoritesList(b); err != nil || len(list) != 0 {
		t.Fatalf("B favorites = %v err=%v, want empty", list, err)
	}
	if store.FavoritesHas(b, id1) || store.FavoritesCount(b) != 0 {
		t.Fatal("B must have no favorites")
	}
	if list, err := store.FavArtistsList(b); err != nil || len(list) != 0 {
		t.Fatalf("B fav artists = %v err=%v, want empty", list, err)
	}
	if list, err := store.FavAlbumsList(b); err != nil || len(list) != 0 {
		t.Fatalf("B fav albums = %v err=%v, want empty", list, err)
	}
	if list, err := store.LaterList(b); err != nil || len(list) != 0 || store.LaterCount(b) != 0 {
		t.Fatalf("B later = %v err=%v, want empty", list, err)
	}
	if ids, err := store.RecentTrackIDs(b, 24, 10); err != nil || len(ids) != 0 {
		t.Fatalf("B recent = %v err=%v, want empty", ids, err)
	}
	if pos, neg, err := store.ListenSignalCounts(b); err != nil || pos != 0 || neg != 0 {
		t.Fatalf("B signals = %d/%d err=%v, want 0/0", pos, neg, err)
	}
	if top, err := store.TopArtists(b, 5); err != nil || len(top) != 0 {
		t.Fatalf("B top artists = %v err=%v, want empty", top, err)
	}
	if top, err := store.TopClusters(b, 5); err != nil || len(top) != 0 {
		t.Fatalf("B top clusters = %v err=%v, want empty", top, err)
	}
	if m, err := store.WeeklyMetrics(b); err != nil || m.Listens7d != 0 || m.ExploreShown != 0 {
		t.Fatalf("B metrics = %+v err=%v, want zeros", m, err)
	}
	if blob, err := store.LatestProfile(b, "global"); err != nil || len(blob) != 0 {
		t.Fatalf("B profile = %dB err=%v, want empty", len(blob), err)
	}
	if tips, err := store.ListDiscoverTips(b, "", 20); err != nil || len(tips) != 0 {
		t.Fatalf("B tips = %v err=%v, want empty", tips, err)
	}
	if pid, name, n, err := store.PlaylistMeta(b, "daily"); err != nil || pid != 0 || name != "" || n != 0 {
		t.Fatalf("B playlist meta = %d/%q/%d err=%v, want zero", pid, name, n, err)
	}
	if pl, err := store.LatestPlaylist(b, "daily"); err != nil || pl != nil {
		t.Fatalf("B latest playlist = %v err=%v, want nil", pl, err)
	}
	if shares, err := store.ListRadioShares(b, true); err != nil || len(shares) != 0 {
		t.Fatalf("B radio shares = %v err=%v, want empty", shares, err)
	}

	// ---------- прямые запросы по чужим id → пусто/не найдено ----------
	if _, ok, err := store.LoadPlaySession(b, "sess-iso-a"); err != nil || ok {
		t.Fatalf("B load A's play session: ok=%v err=%v, want not found", ok, err)
	}
	if err := store.RevokeRadioShare(b, "tok-a"); err == nil {
		t.Fatal("B must not revoke A's radio share")
	}
	if sh, ok, _ := store.GetActiveRadioShare("tok-a"); !ok || !sh.Active {
		t.Fatal("A's share must stay active after B's revoke attempt")
	}
	// мутации B не задевают данные A
	if err := store.FavoritesRemove(b, id1); err != nil {
		t.Fatal(err)
	}
	if err := store.LaterRemove(b, id1); err != nil {
		t.Fatal(err)
	}
	if !store.FavoritesHas(a, id1) || store.FavoritesCount(a) != 2 {
		t.Fatal("A's favorites must be intact after B's mutation")
	}
	if _, err := store.LatestPlaylist(a, "daily"); err != nil {
		t.Fatal(err)
	}

	// ---------- is_admin ≠ доступ к чужим данным ----------
	if _, err := store.DB.Exec(`UPDATE users SET is_admin = TRUE WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}
	if list, err := store.FavoritesList(b); err != nil || len(list) != 0 {
		t.Fatalf("admin B favorites = %v err=%v, want still empty (admin is not access)", list, err)
	}
	if _, ok, err := store.LoadPlaySession(b, "sess-iso-a"); err != nil || ok {
		t.Fatalf("admin B load A's session: ok=%v err=%v, want not found", ok, err)
	}
}

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isoVec(dim int) []byte {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 0.1
	}
	return index.Float32Bytes(v)
}
