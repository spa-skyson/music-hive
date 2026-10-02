// Частично производно от Navidrome (https://github.com/navidrome/navidrome),
// файлы server/subsonic/responses/{responses,errors}.go,
// © 2016—2026 Navidrome contributors. Лицензия GPL-3.0 (см. LICENSE в корне
// репозитория); производный код наследует GPL-3.0.

package subsonic

import (
	"encoding/json"
	"encoding/xml"
)

// Версия протокола, которую заявляет сервер (Subsonic 1.16.1 + OpenSubsonic).
const Version = "1.16.1"

// ServerType — поле "type" конверта: имя продукта.
const ServerType = "music-hive"

const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// Коды ошибок протокола Subsonic (спецификация + errors.go Navidrome).
const (
	ErrorGeneric            int32 = 0  // универсальная ошибка
	ErrorMissingParameter   int32 = 10 // обязательный параметр отсутствует
	ErrorClientTooOld       int32 = 20 // клиенту нужно обновиться
	ErrorServerTooOld       int32 = 30 // серверу нужно обновиться
	ErrorAuthenticationFail int32 = 40 // неверный логин/пароль
	ErrorAuthorizationFail  int32 = 50 // операция не разрешена
	ErrorDataNotFound       int32 = 70 // данные не найдены
)

var errorMessages = map[int32]string{
	ErrorGeneric:            "A generic error",
	ErrorMissingParameter:   "Required parameter is missing",
	ErrorClientTooOld:       "Incompatible Subsonic REST protocol version. Client must upgrade",
	ErrorServerTooOld:       "Incompatible Subsonic REST protocol version. Server must upgrade",
	ErrorAuthenticationFail: "Wrong username or password",
	ErrorAuthorizationFail:  "User is not authorized for the given operation",
	ErrorDataNotFound:       "The requested data was not found",
}

// ErrorMsg — стандартное сообщение кода (для ошибок без своего текста).
func ErrorMsg(code int32) string {
	if m, ok := errorMessages[code]; ok {
		return m
	}
	return errorMessages[ErrorGeneric]
}

// Subsonic — конверт subsonic-response. Один DTO сериализуется и в JSON,
// и в XML (encoding/json + encoding/xml). Эндпоинты F3.2+ добавляют сюда
// свои поля-указатели по образцу License/OpenSubsonicExtensions.
type Subsonic struct {
	XMLName                xml.Name                `xml:"http://subsonic.org/restapi subsonic-response" json:"-"`
	Status                 string                  `xml:"status,attr" json:"status"`
	Version                string                  `xml:"version,attr" json:"version"`
	Type                   string                  `xml:"type,attr" json:"type"`
	ServerVersion          string                  `xml:"serverVersion,attr" json:"serverVersion"`
	OpenSubsonic           bool                    `xml:"openSubsonic,attr,omitempty" json:"openSubsonic,omitempty"`
	Error                  *Error                  `xml:"error,omitempty" json:"error,omitempty"`
	License                *License                `xml:"license,omitempty" json:"license,omitempty"`
	OpenSubsonicExtensions *OpenSubsonicExtensions `xml:"openSubsonicExtensions,omitempty" json:"openSubsonicExtensions,omitempty"`

	// Каталог (F3.2, #24): одна полезная нагрузка на эндпоинт, JSON-имена
	// полей — по спецификации Subsonic.
	MusicFolders *MusicFolders     `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	Indexes      *Indexes          `xml:"indexes,omitempty" json:"indexes,omitempty"`
	Directory    *Directory        `xml:"directory,omitempty" json:"directory,omitempty"`
	Artists      *Artists          `xml:"artists,omitempty" json:"artists,omitempty"`
	Artist       *ArtistWithAlbums `xml:"artist,omitempty" json:"artist,omitempty"`
	Album        *AlbumWithSongs   `xml:"album,omitempty" json:"album,omitempty"`
	Song         *Child            `xml:"song,omitempty" json:"song,omitempty"`
	Genres       *Genres           `xml:"genres,omitempty" json:"genres,omitempty"`
	AlbumList    *AlbumList        `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2   *AlbumList2       `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	RandomSongs  *RandomSongs      `xml:"randomSongs,omitempty" json:"randomSongs,omitempty"`
	SongsByGenre *SongsByGenre     `xml:"songsByGenre,omitempty" json:"songsByGenre,omitempty"`

	// Медиа-аннотации и поиск (F3.3, #25).
	Search2    *Search2    `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	Search3    *Search3    `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	Starred    *Starred    `xml:"starred,omitempty" json:"starred,omitempty"`
	Starred2   *Starred2   `xml:"starred2,omitempty" json:"starred2,omitempty"`
	Lyrics     *Lyrics     `xml:"lyrics,omitempty" json:"lyrics,omitempty"`
	LyricsList *LyricsList `xml:"lyricsList,omitempty" json:"lyricsList,omitempty"`

	// Плейлисты и очередь (F3.4, #26).
	Playlists *Playlists `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Playlist  *Playlist  `xml:"playlist,omitempty" json:"playlist,omitempty"`
	PlayQueue *PlayQueue `xml:"playQueue,omitempty" json:"playQueue,omitempty"`

	// Прочее (F3.4, #26).
	Bookmarks             *Bookmarks             `xml:"bookmarks,omitempty" json:"bookmarks,omitempty"`
	InternetRadioStations *InternetRadioStations `xml:"internetRadioStations,omitempty" json:"internetRadioStations,omitempty"`
	User                  *User                  `xml:"user,omitempty" json:"user,omitempty"`
	Users                 *Users                 `xml:"users,omitempty" json:"users,omitempty"`
	NowPlaying            *NowPlaying            `xml:"nowPlaying,omitempty" json:"nowPlaying,omitempty"`
	ScanStatus            *ScanStatus            `xml:"scanStatus,omitempty" json:"scanStatus,omitempty"`
	SimilarSongs          *SimilarSongs          `xml:"similarSongs,omitempty" json:"similarSongs,omitempty"`
	SimilarSongs2         *SimilarSongs2         `xml:"similarSongs2,omitempty" json:"similarSongs2,omitempty"`
}

// Search2 — ответ search2 (блоки как у search3, имя элемента другое).
type Search2 = SearchResult

// Search3 — ответ search3.
type Search3 = SearchResult

// jsonWrapper — JSON-обёртка {"subsonic-response": {...}} (XML самодостатен).
type jsonWrapper struct {
	Subsonic *Subsonic `json:"subsonic-response"`
}

// Error — элемент <error code message/> конверта.
type Error struct {
	Code    int32  `xml:"code,attr" json:"code"`
	Message string `xml:"message,attr" json:"message"`
}

// License — ответ getLicense: у нас валидна всегда.
type License struct {
	Valid bool   `xml:"valid,attr" json:"valid"`
	Email string `xml:"email,attr,omitempty" json:"email,omitempty"`
}

// OpenSubsonicExtension — реализованное расширение OpenSubsonic.
type OpenSubsonicExtension struct {
	Name     string  `xml:"name,attr" json:"name"`
	Versions []int32 `xml:"versions" json:"versions"`
}

// Array — срез, который в JSON сериализуется как [], а не null: пустые
// списки Subsonic-клиенты ожидают массивами (Array[T] из Navidrome).
type Array[T any] []T

func (a Array[T]) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(a))
}

// OpenSubsonicExtensions — список getOpenSubsonicExtensions.
type OpenSubsonicExtensions = Array[OpenSubsonicExtension]
