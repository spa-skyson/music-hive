package db

import "time"

// Backend — контракт хранилища; реализация — PostgreSQL (*PGStore).
// Верхние слои (api/playback/queue/recommend/app) работают только через
// него; main открывает PG по MUSIC_HIVE_DATABASE_URL.
//
// User-скопинг (F2.2, GitLab #20): user-scoped методы принимают userID
// первым параметром. PGStore фильтрует по user_id (дизайн D6 — без RLS).
// userID=0 — владелец по умолчанию (boot-пути). Каталог
// (tracks/artists/albums), lyrics, jobs, transitions и публичный
// share-listen — общие, без userID.
type Backend interface {
	Close() error

	// Каталог.
	LoadReadyTracks() ([]TrackRow, error)
	// ReadyTrackCount — count тех же строк, что отдаёт LoadReadyTracks,
	// без загрузки метаданных/векторов (probe-цикл #47 сверяет его
	// с индексом, чтобы поймать инкрементальный прогресс длинной job).
	ReadyTrackCount() (int, error)
	ListCatalogTracks() ([]CatalogTrack, error)
	TrackPath(id int64) (string, error)

	// TrackArtworkPath — artwork_path активного трека; false = трека нет
	// или обложки нет. Резолвит по БД, а не по индексу: индекс содержит
	// только треки с готовыми эмбеддингами, обложки не должны от них
	// зависеть (прод-баг: во время первичного скана все 404).
	TrackArtworkPath(id int64) (string, bool)

	// История прослушивания.
	RecentTrackIDs(userID int64, hours, limit int) ([]int64, error)
	InsertListen(userID int64, trackID int64, action, source, sessionID, reason string,
		position, duration, listened *float64) (int64, error)
	BumpTransition(fromID, toID int64, weight float64) error
	BumpRecStats(userID int64, trackID int64, shown, skipEarly, completed int) error
	InsertRecommendationImpressions(userID int64, items []RecommendationImpression) error

	// Джобы.
	EnqueueJob(kind, payloadJSON string) (int64, error)
	// FindActiveJob — последняя джобa kind в статусе pending/running (nil — нет);
	// guard от повторной постановки того же kind (#56).
	FindActiveJob(kind string) (*Job, error)
	ListDoneJobsAfter(after string, limit int) ([]Job, error)
	ListJobs(status string, limit int) ([]Job, error)
	GetJob(id int64) (*Job, error)

	// Плейлисты / избранное / отложенное / подсказки.
	PlaylistMeta(userID int64, kind string) (id int64, name string, n int, err error)
	LaterList(userID int64) ([]PlaylistTrack, error)
	LaterAdd(userID int64, trackID int64) error
	LaterRemove(userID int64, trackID int64) error
	LaterCount(userID int64) int
	FavoritesList(userID int64) ([]PlaylistTrack, error)
	FavoritesAdd(userID int64, trackID int64) error
	FavoritesRemove(userID int64, trackID int64) error
	FavoritesCount(userID int64) int
	FavoritesHas(userID int64, trackID int64) bool
	FavArtistsList(userID int64) ([]FavArtist, error)
	FavArtistAdd(userID int64, artist string) error
	FavArtistRemove(userID int64, artist string) error
	FavArtistHas(userID int64, artist string) bool
	FavArtistCount(userID int64) int
	FavAlbumsList(userID int64) ([]FavAlbum, error)
	FavAlbumAdd(userID int64, artist, album string) error
	FavAlbumRemove(userID int64, artist, album string) error
	FavAlbumHas(userID int64, artist, album string) bool
	FavAlbumCount(userID int64) int
	LatestPlaylist(userID int64, kind string) (*Playlist, error)
	ListDiscoverTips(userID int64, kind string, limit int) ([]DiscoverTip, error)

	// Сессии плеера и граф переходов.
	// LoadPlaySession скопирован по пользователю (чужая сессия = не найдена);
	// остальное по play_sessions — глобальная GC-чистина, без userID.
	LoadTransitionGraph() (map[int64]map[int64]float64, error)
	UpsertPlaySession(row PlaySessionRow) error
	LoadPlaySession(userID int64, id string) (PlaySessionRow, bool, error)
	DeletePlaySession(id string) error
	DeleteStalePlaySessions(olderThan time.Time) error
	CountPlaySessions() (int, error)
	OldestPlaySessionIDs(limit int) ([]string, error)

	// Share-radio: создание/лист/отзыв — от имени пользователя;
	// GetActiveRadioShare/TouchRadioShareListen — публичный путь по токену.
	CreateRadioShare(userID int64, token, name string) (RadioShare, error)
	ListRadioShares(userID int64, includeRevoked bool) ([]RadioShare, error)
	GetActiveRadioShare(token string) (RadioShare, bool, error)
	RevokeRadioShare(userID int64, token string) error
	TouchRadioShareListen(token string) error

	// Тексты.
	GetLyrics(trackID int64) (*Lyrics, bool, error)

	// Профиль вкуса и статистика.
	SaveProfile(userID int64, context string, emb []byte) error
	PruneProfiles(userID int64, context string, keep int) error
	LatestProfile(userID int64, context string) ([]byte, error)
	ListenSignalCounts(userID int64) (pos, neg int, err error)
	TopArtists(userID int64, limit int) ([]ArtistCount, error)
	TopClusters(userID int64, limit int) ([]ClusterCount, error)

	// Метрики.
	WeeklyMetrics(userID int64) (Metrics, error)
}

var _ Backend = (*PGStore)(nil)
