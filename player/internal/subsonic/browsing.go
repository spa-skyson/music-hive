// Browse-эндпоинты Subsonic: ID3 (getArtists/getArtist/getAlbum/getSong)
// и legacy-дерево (getIndexes/getMusicDirectory) + getMusicFolders/getGenres.
// DTO и формы ответов производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/{browsing,responses}.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- DTO

// MusicFolders — ответ getMusicFolders: одна библиотека.
type MusicFolders struct {
	Folder Array[MusicFolder] `xml:"musicFolder" json:"musicFolder"`
}

// MusicFolder — корень библиотеки.
type MusicFolder struct {
	ID   string `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

// Artists — обёртка getArtists (ID3): index-блоки по первой букве.
type Artists struct {
	Index Array[ArtistIndex] `xml:"index" json:"index"`
}

// ArtistIndex — блок «A»..«Z» / «#».
type ArtistIndex struct {
	Name   string           `xml:"name,attr" json:"name"`
	Artist Array[ArtistID3] `xml:"artist" json:"artist"`
}

// ArtistID3 — артист ID3-ответов (getArtists/getArtist).
type ArtistID3 struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
	CoverArt   string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
}

// ArtistWithAlbums — ответ getArtist: артист + его альбомы.
type ArtistWithAlbums struct {
	ArtistID3
	Album Array[AlbumID3] `xml:"album" json:"album"`
}

// AlbumID3 — альбом ID3-ответов (getArtist/getAlbum/getAlbumList2).
type AlbumID3 struct {
	ID        string `xml:"id,attr" json:"id"`
	Name      string `xml:"name,attr" json:"name"`
	Artist    string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	ArtistID  string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt  string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount int    `xml:"songCount,attr,omitempty" json:"songCount,omitempty"`
	Duration  int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	Created   string `xml:"created,attr,omitempty" json:"created,omitempty"`
	Year      int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Genre     string `xml:"genre,attr,omitempty" json:"genre,omitempty"`
}

// AlbumWithSongs — ответ getAlbum: альбом + треки.
type AlbumWithSongs struct {
	AlbumID3
	Song Array[Child] `xml:"song" json:"song"`
}

// Child — элемент медиатеки (песня или псевдо-директория legacy-дерева);
// имя XML-элемента задаёт поле конверта (song/child/album).
type Child struct {
	ID          string `xml:"id,attr" json:"id"`
	Parent      string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool   `xml:"isDir,attr" json:"isDir"`
	Title       string `xml:"title,attr" json:"title"`
	Album       string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	Year        int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	Size        int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	ContentType string `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	CoverArt    string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Duration    int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	BitRate     int    `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	Path        string `xml:"path,attr,omitempty" json:"path,omitempty"`
	DiscNumber  int    `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
	Created     string `xml:"created,attr,omitempty" json:"created,omitempty"`
	AlbumID     string `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string `xml:"type,attr,omitempty" json:"type,omitempty"`
}

// Indexes — legacy getIndexes: артисты как псевдо-директории ar-<id>.
type Indexes struct {
	LastModified int64        `xml:"lastModified,attr,omitempty" json:"lastModified,omitempty"`
	Index        Array[Index] `xml:"index" json:"index"`
}

// Index — блок legacy-индекса.
type Index struct {
	Name   string             `xml:"name,attr" json:"name"`
	Artist Array[IndexArtist] `xml:"artist" json:"artist"`
}

// IndexArtist — артист-псевдо-директория (id с префиксом ar-).
type IndexArtist struct {
	ID   string `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

// Directory — legacy getMusicDirectory: содержимое узла дерева.
type Directory struct {
	ID    string       `xml:"id,attr" json:"id"`
	Name  string       `xml:"name,attr" json:"name"`
	Child Array[Child] `xml:"child" json:"child"`
}

// Genres — ответ getGenres.
type Genres struct {
	Genre Array[Genre] `xml:"genre" json:"genre"`
}

// Genre — жанр: имя в тексте элемента (не в атрибуте).
type Genre struct {
	SongCount  int    `xml:"songCount,attr" json:"songCount"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
	Value      string `xml:",chardata" json:"value"`
}

// ---------------------------------------------------------------- helpers

// dirID — id псевдо-директории legacy-дерева: ar-<artistID> / al-<albumID>
// (внутренняя конвенция, entity-ID остаются голыми числами).
func dirID(prefix string, id int64) string {
	return prefix + "-" + strconv.FormatInt(id, 10)
}

// isoTime — ISO 8601 для атрибутов created; пустая строка для zero-времени.
func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// audioMimeByExt — детерминированный mime по расширению (mime.TypeByExtension
// зависит от /etc/mime.types хоста, golden-тесты требуют стабильности).
var audioMimeByExt = map[string]string{
	".flac": "audio/flac", ".mp3": "audio/mpeg", ".ogg": "audio/ogg",
	".oga": "audio/ogg", ".opus": "audio/ogg", ".wav": "audio/x-wav",
	".m4a": "audio/mp4", ".aac": "audio/aac", ".wma": "audio/x-ms-wma",
	".aiff": "audio/x-aiff", ".ape": "audio/x-ape",
}

func contentTypeByExt(ext string) string {
	if ct, ok := audioMimeByExt[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// newArtistID3 — ArtistRow → ArtistID3 (coverArt ar-<id>).
func newArtistID3(a ArtistRow) ArtistID3 {
	return ArtistID3{
		ID:         strconv.FormatInt(a.ID, 10),
		Name:       a.Name,
		AlbumCount: a.AlbumCount,
		CoverArt:   dirID("ar", a.ID),
	}
}

// newAlbumID3 — AlbumRow → AlbumID3 (coverArt al-<id>).
func newAlbumID3(a AlbumRow) AlbumID3 {
	return AlbumID3{
		ID:        strconv.FormatInt(a.ID, 10),
		Name:      a.Title,
		Artist:    a.Artist,
		ArtistID:  strconv.FormatInt(a.ArtistID, 10),
		CoverArt:  dirID("al", a.ID),
		SongCount: a.SongCount,
		Duration:  a.Duration,
		Created:   isoTime(a.CreatedAt),
		Year:      a.Year,
		Genre:     a.Genre,
	}
}

// songChild — SongRow → Child. parent: голый albumId в ID3-ответах,
// al-<albumId> внутри legacy-дерева (навигация «вверх» клиента).
func songChild(s SongRow, parent string) Child {
	ext := strings.ToLower(filepath.Ext(s.Path))
	c := Child{
		ID:          strconv.FormatInt(s.ID, 10),
		Parent:      parent,
		Title:       s.Title,
		Artist:      s.Artist,
		Album:       s.Album,
		Track:       s.Track,
		Year:        s.Year,
		Size:        s.Size,
		ContentType: contentTypeByExt(ext),
		Suffix:      strings.TrimPrefix(ext, "."),
		CoverArt:    dirID("mf", s.ID),
		Duration:    s.Duration,
		BitRate:     s.Bitrate,
		Path:        s.Path,
		DiscNumber:  s.DiscNumber,
		Created:     isoTime(s.CreatedAt),
		Type:        "music",
	}
	if s.AlbumID != 0 {
		c.AlbumID = strconv.FormatInt(s.AlbumID, 10)
	}
	if s.ArtistID != 0 {
		c.ArtistID = strconv.FormatInt(s.ArtistID, 10)
	}
	return c
}

// albumDirChild — альбом как псевдо-директория al-<id> (legacy-дерево и
// getAlbumList).
func albumDirChild(a AlbumRow) Child {
	return Child{
		ID:       dirID("al", a.ID),
		Parent:   dirID("ar", a.ArtistID),
		IsDir:    true,
		Title:    a.Title,
		Artist:   a.Artist,
		Year:     a.Year,
		CoverArt: dirID("al", a.ID),
		Created:  isoTime(a.CreatedAt),
	}
}

// parseEntityID — обязательный числовой id-параметр: отсутствие → code 10,
// мусорное значение → code 70.
func parseEntityID(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, NewError(ErrorMissingParameter, "required parameter "+name+" is missing")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, NewError(ErrorDataNotFound, "")
	}
	return id, nil
}

// indexLetter — блок индекса по name_norm: A..Z, прочее (цифры, кириллица) — #.
func indexLetter(nameNorm string) string {
	if r := []rune(nameNorm); len(r) > 0 {
		if c := r[0]; c >= 'a' && c <= 'z' {
			return string(rune(c - 32))
		}
	}
	return "#"
}

// groupByLetter — артисты по блокам A..Z,# (вход уже отсортирован по
// name_norm; внутри блока порядок сохраняется). Возвращает имена блоков
// в порядке A..Z,# и карту содержимого.
func groupByLetter(list []ArtistRow) ([]string, map[string][]ArtistRow) {
	byLetter := make(map[string][]ArtistRow)
	for _, a := range list {
		l := indexLetter(a.NameNorm)
		byLetter[l] = append(byLetter[l], a)
	}
	var letters []string
	for c := 'A'; c <= 'Z'; c++ {
		if _, ok := byLetter[string(c)]; ok {
			letters = append(letters, string(c))
		}
	}
	if _, ok := byLetter["#"]; ok {
		letters = append(letters, "#")
	}
	return letters, byLetter
}

// ---------------------------------------------------------------- handlers

// getMusicFolders — одна папка {id:"1", name:"music"} (каталог общий).
func (rt *Router) getMusicFolders(_ *http.Request) (*Subsonic, error) {
	resp := rt.NewResponse()
	resp.MusicFolders = &MusicFolders{Folder: Array[MusicFolder]{{ID: "1", Name: "music"}}}
	return resp, nil
}

// getArtists — ID3-индекс артистов: блоки A..Z/#, у каждого артиста
// albumCount и coverArt ar-<id>. ignoredArticles не нужен (сканер их не хранит).
func (rt *Router) getArtists(_ *http.Request) (*Subsonic, error) {
	artists, err := rt.Catalog.SubsonicArtists()
	if err != nil {
		return nil, err
	}
	letters, byLetter := groupByLetter(artists)
	index := make(Array[ArtistIndex], 0, len(letters))
	for _, l := range letters {
		block := ArtistIndex{Name: l}
		for _, a := range byLetter[l] {
			block.Artist = append(block.Artist, newArtistID3(a))
		}
		index = append(index, block)
	}
	resp := rt.NewResponse()
	resp.Artists = &Artists{Index: index}
	return resp, nil
}

// getArtist — артист + его альбомы (по году); 70 если артиста нет.
func (rt *Router) getArtist(r *http.Request) (*Subsonic, error) {
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	artist, found, err := rt.Catalog.SubsonicArtist(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, NewError(ErrorDataNotFound, "")
	}
	albums, err := rt.Catalog.SubsonicArtistAlbums(id)
	if err != nil {
		return nil, err
	}
	aw := ArtistWithAlbums{ArtistID3: newArtistID3(artist), Album: Array[AlbumID3]{}}
	for _, a := range albums {
		aw.Album = append(aw.Album, newAlbumID3(a))
	}
	resp := rt.NewResponse()
	resp.Artist = &aw
	return resp, nil
}

// getAlbum — альбом + треки (song: parent=albumId, coverArt mf-<trackId>);
// 70 если альбома нет.
func (rt *Router) getAlbum(r *http.Request) (*Subsonic, error) {
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	album, found, err := rt.Catalog.SubsonicAlbum(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, NewError(ErrorDataNotFound, "")
	}
	songs, err := rt.Catalog.SubsonicAlbumSongs(id)
	if err != nil {
		return nil, err
	}
	aw := AlbumWithSongs{AlbumID3: newAlbumID3(album), Song: Array[Child]{}}
	parent := strconv.FormatInt(id, 10)
	for _, s := range songs {
		aw.Song = append(aw.Song, songChild(s, parent))
	}
	resp := rt.NewResponse()
	resp.Album = &aw
	return resp, nil
}

// getSong — один трек как Child; 70 если нет.
func (rt *Router) getSong(r *http.Request) (*Subsonic, error) {
	id, err := parseEntityID(r, "id")
	if err != nil {
		return nil, err
	}
	song, found, err := rt.Catalog.SubsonicSong(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, NewError(ErrorDataNotFound, "")
	}
	child := songChild(song, strconv.FormatInt(song.AlbumID, 10))
	resp := rt.NewResponse()
	resp.Song = &child
	return resp, nil
}

// getIndexes — legacy-индекс: артисты как псевдо-директории ar-<id>
// (musicFolderId/ignoredArticles игнорируем — папка одна).
func (rt *Router) getIndexes(_ *http.Request) (*Subsonic, error) {
	artists, err := rt.Catalog.SubsonicArtists()
	if err != nil {
		return nil, err
	}
	letters, byLetter := groupByLetter(artists)
	index := make(Array[Index], 0, len(letters))
	for _, l := range letters {
		block := Index{Name: l}
		for _, a := range byLetter[l] {
			block.Artist = append(block.Artist, IndexArtist{
				ID:   dirID("ar", a.ID),
				Name: a.Name,
			})
		}
		index = append(index, block)
	}
	resp := rt.NewResponse()
	resp.Indexes = &Indexes{Index: index}
	return resp, nil
}

// getMusicDirectory — эмуляция файлового дерева: ar-<id> → альбомы-чилды
// (al-<id>), al-<id> → треки-чилды. Всё прочее — 70.
func (rt *Router) getMusicDirectory(r *http.Request) (*Subsonic, error) {
	raw := r.URL.Query().Get("id")
	if raw == "" {
		return nil, NewError(ErrorMissingParameter, "required parameter id is missing")
	}
	if artistRaw, ok := strings.CutPrefix(raw, "ar-"); ok {
		id, err := parseDirID(artistRaw)
		if err != nil {
			return nil, err
		}
		artist, found, err := rt.Catalog.SubsonicArtist(id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, NewError(ErrorDataNotFound, "")
		}
		albums, err := rt.Catalog.SubsonicArtistAlbums(id)
		if err != nil {
			return nil, err
		}
		dir := Directory{ID: raw, Name: artist.Name, Child: Array[Child]{}}
		for _, a := range albums {
			dir.Child = append(dir.Child, albumDirChild(a))
		}
		resp := rt.NewResponse()
		resp.Directory = &dir
		return resp, nil
	}
	if albumRaw, ok := strings.CutPrefix(raw, "al-"); ok {
		id, err := parseDirID(albumRaw)
		if err != nil {
			return nil, err
		}
		album, found, err := rt.Catalog.SubsonicAlbum(id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, NewError(ErrorDataNotFound, "")
		}
		songs, err := rt.Catalog.SubsonicAlbumSongs(id)
		if err != nil {
			return nil, err
		}
		dir := Directory{ID: raw, Name: album.Title, Child: Array[Child]{}}
		for _, s := range songs {
			dir.Child = append(dir.Child, songChild(s, raw)) // parent = al-<id>
		}
		resp := rt.NewResponse()
		resp.Directory = &dir
		return resp, nil
	}
	return nil, NewError(ErrorDataNotFound, "")
}

// parseDirID — числовая часть dir-ID после префикса; мусор → 70.
func parseDirID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, NewError(ErrorDataNotFound, "")
	}
	return id, nil
}

// getGenres — жанр + songCount (track_genres) + albumCount (albums.genre).
func (rt *Router) getGenres(_ *http.Request) (*Subsonic, error) {
	genres, err := rt.Catalog.SubsonicGenres()
	if err != nil {
		return nil, err
	}
	list := make(Array[Genre], 0, len(genres))
	for _, g := range genres {
		list = append(list, Genre{SongCount: g.SongCount, AlbumCount: g.AlbumCount, Value: g.Name})
	}
	resp := rt.NewResponse()
	resp.Genres = &Genres{Genre: list}
	return resp, nil
}
