# Слой Subsonic `/rest/` — покрытие, маппинг, авторизация

Приложение к [`target-architecture.md`](target-architecture.md) (§8). Целевая версия протокола: **Subsonic 1.16.1 + OpenSubsonic-расширения**. Заявляемые расширения: `tokenAuthentication` (Bearer на `/rest/*`).

Проверяемые клиенты: **Symfonium** (JSON), **DSub** (XML), **Ultrasonic** (JSON), **Feishin** (JSON). Формат ответа: параметр `f=json|xml`; при отсутствии `f` — XML (по спецификации). Один набор DTO с json- и xml-тегами.

## 1. Конверт и общие правила

```json
{ "subsonic-response": {
    "status": "ok",                  // | "failed"
    "version": "1.16.1",
    "type": "music-hive",
    "serverVersion": "<player version>",
    "openSubsonic": true,
    ...полезная нагрузка эндпоинта...
} }
```

Ошибки: `status:"failed"` + код из спецификации (0 generic, 10 missing param, 20 incompatible version, 30 incompatible protocol, 40 wrong username/password, 50 token expired, 70 trial expired не используем).

**Авторизация на `/rest/*`** (порядок проверки):

1. Наша cookie `music_hive_session` (браузерные клиенты) — как `/api/*`.
2. `Authorization: Bearer <api_token>` (OpenSubsonic tokenAuthentication) — резолв через `api_tokens`.
3. `u` + `t` + `s` (token+salt): `t == md5_hex(subsonic_md5 + s)`.
4. `u` + `p` (plain или `enc:`-hex): argon2id-проверка.

Отключается целиком пунктом 3 `MUSIC_HIVE_SUBSONIC_LEGACY_AUTH=0` (§7.2 основного документа). Rate-limit — общий с `/api/auth/login` (per-IP).

## 2. Маппинг идентификаторов и полей

| Subsonic | Наш источник | Примечания |
|---|---|---|
| `artistID` | `artists.id` | стабильны между rescan (upsert по `name_norm`) |
| `albumID` | `albums.id` | `(artist_id, title_norm)` |
| `songID` | `tracks.id` | старые id переносятся импортёром |
| `coverArt` | `ar-<artistID>` / `al-<albumID>` / `mf-<trackID>` | префиксы только рендера; сам getCoverArt — #25 |
| `musicFolder` | один, `id=1` | корень библиотеки |
| `albumArtist` | `album_artist_id` (fallback artist) | корректные компиляции |
| `discNumber` | `tracks.disc_number` | новая колонка F1 |
| `genre` (song) | первый из `track_genres` | |
| `genre` (album/artist) | мажоритарный по трекам | считается запросом |
| `playCount` | `count(listening_history action=finish)` | per-user; индексы покрывают |
| `created` | `tracks.created_at` / `albums.created_at` | для `type=newest` |
| `contentType`/`suffix` | по расширению пути | |
| `duration` (сек), `bitRate` (kbps) | `tracks` | |
| `starred` | `user_favorites` | timestamp добавления |
| `userRating`/`averageRating` | `user_ratings` | average по всем пользователям |
| `mbid` | `tracks/albums/artists.mbid` | если есть в тегах |

**Транскодинг** (`stream`, F3.3): `maxBitRate` (kbps, 0=без лимита) + `format` — оригинал при `format=raw`/пустом и не превышении битрейта, иначе отдаётся мобильный кеш-вариант (`/api/stream?q=mobile`: `MUSIC_HIVE_MOBILE_FORMAT`/`MUSIC_HIVE_MOBILE_BITRATE`; ponytail-упрощение — запрошенный формат/битрейт не подставляются, отдельные профили когда клиенты реально попросят). HTTP Range — как в текущем стриминге (200/206). `timeOffset` (видео-параметр) игнорируется. `stream`/`download` не пишут plays — история только через `scrobble`.

## 3. Таблица покрытия эндпоинтов

Сложность: S — прямая выборка; M — новая логика/агрегация.

### Системные

| Эндпоинт | Источник данных | Сложность | Примечания |
|---|---|---|---|
| `ping` | — | S | auth-проба |
| `getLicense` | константа | S | `license:true` |
| `getMusicFolders` | константа | S | одна папка |
| `getScanStatus` | `jobs` (kind=scan/full_rescan) + `count(tracks)` | S | `scanning` если running |
| `startScan` | enqueue job `scan` | S | admin-only |
| `getUser`/`getUsers` | `users` | S | adminRole=`is_admin` |
| `getOpenSubsonicExtensions` | константа | S | `tokenAuthentication` |

### Навигация (ID3 + файловая)

| Эндпоинт | Источник | Сложность | Примечания |
|---|---|---|---|
| `getArtists` (ID3) | `artists` + counts | M | группировка по первой букве `sort_name` |
| `getArtist` | `albums` по артисту | S | |
| `getAlbum` | `tracks` по альбому | S | |
| `getSong` | `tracks` | S | |
| `getIndexes` (не-ID3) | те же `artists` | M | та же структура, `lastModified` из max(updated_at) |
| `getMusicDirectory` | артист → альбомы; альбом → треки | M | полиморфный id |
| `getGenres` | `genres` + counts | S | |
| `getAlbumList2` | `albums` | M | `newest`/`recent`/`frequent`/`random`/`starred`/`byGenre`/`byYear`/`alphabeticalByName|Artist` |
| `getRandomSongs` | `tracks` `ORDER BY random()` + фильтры genre/fromYear/toYear | S | |
| `getSongsByGenre` | `track_genres` | S | пагинация |
| `search3` | ILIKE по artists/albums/tracks + rank | M | artistCount/albumCount/songCount/offset |
| `getArtistInfo2` | `emb_*_groups` (похожие артисты) | M | biography/lastFm — пустые элементы |
| `getAlbumInfo2` | `albums` | S | note из meta |
| `getSimilarSongs2` | ANN от центроида артиста/альбома | M | count |
| `getTopSongs` | `listening_history` по артисту | M | |

Типы album-списков F3.3: `starred` → `user_favorites` текущего пользователя;
`frequent` → `SUM(user_track_stats.completed)` пользователя; `highest` →
`AVG(user_ratings)` по всем пользователям; `recent` → `MAX(tracks.created_at)`
(добавленная музыка; отступление от спецификации «recently played» —
просмотренное пользователем намеренно не смешиваем с новинками каталога,
по ТЗ #25). Без данных — пустой список, не ошибка.

### Медиа

| Эндпоинт | Источник | Сложность | Примечания |
|---|---|---|---|
| `stream` | `/api/stream` логика | M | Range + транскод (§2) |
| `download` | оригинал файла | S | |
| `getCoverArt` | artwork-кеш + `size` (ffmpeg scale) | S | кеширование по ETag |

Решения F3.3 по `getCoverArt`: `al-<id>` → `albums.cover_track_id` (fallback — первый
трек альбома с `artwork_path` по disc/track), `ar-<id>` → обложка первого альбома
артиста (колонки artwork у артистов в схеме нет), `mf-<id>` → `tracks.artwork_path`.
`size` — ресайз общим artwork-механизмом (clamp 48..512, JPEG-кеш `<trackID>_w<size>.jpg`);
нет обложки/сущности — код 70.

### Оценки и история

| Эндпоинт | Источник | Сложность | Примечания |
|---|---|---|---|
| `getStarred2` | `user_favorites` | S | три списка: артисты/альбомы/песни |
| `star`/`unstar` | `user_favorites` | S | мульти-id параметры (id/albumId/artistId[]) |
| `setRating` | `user_ratings` | S | 0 = удаление |
| `scrobble` | `listening_history` | S | `ids[]`, `submission` (true→finish, false→now-playing), `time[]` мс |
| `getNowPlaying` | `play_sessions` (current per-user) | S | |

Решения F3.3: `scrobble` пишет строку `listening_history`
(`action=track_end`, `source=subsonic`, `reason=completed`) **и** инкремент
`user_track_stats.completed` (как `/api/events track_end`) — иначе frequent не видел
бы subsonic-прослушиваний; `submission=false` — ok без записи (now-playing не хранится).
`setRating`: колонки `user_ratings` уже в схеме 00001 — миграция не потребовалась; kind
(id трека/альбома/артиста в протоколе не различается) выводится наличием в
`tracks`→`albums`→`artists` (как Navidrome). `getStarred`/`getStarred2` —
`user_favorites` текущего пользователя, свежие звёзды первыми.

### Плейлисты и очередь

| Эндпоинт | Источник | Сложность | Примечания |
|---|---|---|---|
| `getPlaylists` | `playlists` | S | свои; владелец чужих не видит |
| `getPlaylist` | + `playlist_tracks` | S | свой или админ |
| `createPlaylist` | `playlists` | M | name, songId[]; playlistId+songId[] = замена состава |
| `updatePlaylist` | `playlists`/`playlist_tracks` | M | name/comment, добавление/удаление по songIndexToRemove |
| `deletePlaylist` | `playlists` | S | владелец/admin |
| `getPlayQueue` | `play_sessions` | M | маппинг our-queue ↔ Subsonic-очередь (position, current) |
| `savePlayQueue` | `play_sessions` | M | |

Решения F3.4: плейлисты user-scoped (`owner_user_id`, F2.2), kind='user'
у клиентских (системные mix_* тоже показываются — это плейлисты
пользователя); `comment` — в `playlists.meta_json` (`{"comment": ...}`),
`changed` = `created` (колонки updated_at нет); songIndexToRemove —
позиции текущего состава, после применения состав перенумеровывается.
Play-queue — одна на пользователя: строка `play_sessions` с фиксированным
id `subsonic-<userID>` (mode='subsonic'), состояние — JSON в существующем
`queue_json` (`{ids, position, changed, changedBy}`), текущий трек —
`current_id`; новых колонок/таблиц нет. ponytail-потолок: строка живёт по
общему GC play_sessions (7 дней без save / вытеснение из 64 последних) —
забытая очередь = пустая, не ошибка.

### Тексты, закладки, интернет-радио

| Эндпоинт | Источник | Сложность | Примечания |
|---|---|---|---|
| `getLyrics` | `lyrics.plain_lyrics` | S | artist/title из параметров → трек |
| `getLyricsBySongId` (OpenSubsonic) | `lyrics` (synced LRC → structured) | M | |
| `getBookmarks` | `user_listen_later` | S | «послушать позже» = закладка |
| `createBookmark`/`deleteBookmark` | `user_listen_later` | S | position игнорируем/сохраяем в meta |
| `getInternetRadioStations` | `radio_shares` владельца | S | `streamUrl` = PublicBaseURL + `/listen/<token>.mp3` |
| `createInternetRadioStation` | `radio_shares` | S | admin: создаёт share-токен |
| `updateInternetRadioStation`/`deleteInternetRadioStation` | `radio_shares` | S | |

F3.3: `getLyrics` — точный ILIKE-матч `tracks.artist`+`tracks.title`, приоритет
готовых (`status=ready`); нет — пустой `<lyrics/>`. `getLyricsBySongId` —
`structuredLyrics` songLyrics v1: `synced=true` из LRC (`[mm:ss.xx]` → start в мс),
`synced=false` из `plain_lyrics`; `lang="und"` (языка в данных нет). Нет текста —
`lyricsList` с пустым массивом (ok).

Решения F3.4 по закладкам и радио: `user_listen_later.position` — порядок
списка, а не мс внутри трека → `getBookmarks` отдаёт position=0, comment
не хранится (нет колонки); отдельного `getBookmark`-эндпоинта в
спецификации нет — только список/создание/удаление. Станция = активный
share: `createInternetRadioStation` игнорирует streamUrl/homepageUrl
клиента (внешние URL негде хранить) и заводит share-токен, стрим всегда
наш `/listen/<token>.mp3`; update — переименование (owner/admin, гость —
50); delete — отзыв share (история listen_count остаётся), повторный
delete — 70. `getSimilarSongs(2)` — id разбирается как трек → артист →
альбом; похожесть — recommend-слой (ANN от вектора трека / центроида
артиста/альбома) через адаптер в api (subsonic не импортирует index —
цикл через db); count по умолчанию 50. `getTopSongs` — 501 (данных
популярности исполнителя нет). `getAvatar` — 70 (аватаров нет).
`getUser` — свой блок (username≠свой без admin → 50), `getUsers` — только
админ; роли: adminRole/shareRole = is_admin(+owner), settings/stream/
download/playlist/comment/scrobbling = true, jukebox/upload/coverArt/
podcast/videoConversion = false, folder=[1]. `getScanStatus` — count
активных треков + scanning по pending/running jobs scan/full_rescan;
`startScan` — админ, enqueue `full_rescan`. `getNowPlaying` — сессии
`play_sessions`, обновлённые за 10 минут (subsonic-строки очередей
исключены), minutesAgo от updated_at, playerId=0.

### Явно не реализуется

`getShares`* (семантика share-треков ≠ наше share-radio; наш маппинг — internet radio), `jukebox`*, `hls.m3u8`, `getVideos`/`videoConversion`, `getPodcasts`*/`downloadEpisode`, `getChatMessages`/`addChatMessage`, `getWallpapers`, `getPlaylists`- Podcast-ветки, `scanStatus` для видео. Ответ — стандартная ошибка протокола (code 0 / «not implemented»), клиенты её переваривают.

`getTopSongs` (F3.4) — 501: данных популярности исполнителя нет (верхнеуровневых топов по артисту listening_history не даёт, а «дёшево» честного топа нет). `getAvatar` (F3.4) — 70: аватаров нет. Оба статуса финальные для F3.4.

### Статус реализации

- **Готово (F3.1, #23)**: ping, getLicense, getOpenSubsonicExtensions; конверт JSON/XML/jsonp, коды ошибок, POST-мерж, аутентификация u+t+s / u+p (subsonic-пароль — PUT `/api/auth/subsonic-password`).
- **Готово (F3.2, #24)**: getMusicFolders, getArtists, getArtist, getAlbum, getSong (теперь с `path`/`coverArt`), getIndexes, getMusicDirectory (эмуляция дерева `ar-<id>`/`al-<id>`), getGenres, getAlbumList/getAlbumList2 (SQL-типы random/newest/alphabeticalByName/alphabeticalArtist/byYear/byGenre), getRandomSongs, getSongsByGenre.
- **Готово (F3.3, #25)**: stream/download (media-слой `/api/stream`, Range 200/206, транскод в мобильный профиль), getCoverArt (`al-`/`ar-`/`mf-` + size), scrobble (+ user_track_stats), star/unstar, setRating (`user_ratings`, миграция не потребовалась), getStarred/getStarred2, search2/search3 (ILIKE + счётчики блоков), getLyrics/getLyricsBySongId (OpenSubsonic songLyrics), album-списки starred/frequent/highest/recent (см. решения выше).
- **Готово (F3.4, #26)**: getPlaylists/getPlaylist/createPlaylist/updatePlaylist/deletePlaylist (user-scoped, comment в meta_json), getPlayQueue/savePlayQueue (play_sessions, строка `subsonic-<userID>`), getBookmarks/createBookmark/deleteBookmark (user_listen_later, position=0), getInternetRadioStations + create/update/delete (radio_shares, streamUrl = `/listen/<token>.mp3`, права owner/admin), getUser/getUsers (роли по матрице), getScanStatus/startScan (jobs), getNowPlaying (play_sessions ≤10 мин), getSimilarSongs/getSimilarSongs2 (recommend: трек/артист/альбом — все три сида дёшевы, отдельного 501 не осталось).
- **Зарегистрировано, ошибка протокола**: search (legacy-форма v1.0 — 501, живые клиенты не используют), getTopSongs (501), getAvatar (70), прочие из списка «явно не реализуется».
- SQLite-легаси: всё `/rest/*` — заглушка «subsonic requires PostgreSQL» (HTTP 501).

Итог F3.4: **реализовано 21 эндпоинт** (5 плейлистов, 2 play-queue, getNowPlaying, 3 закладки, 4 радио, getUser/getUsers, getScanStatus/startScan, getSimilarSongs(2)); **501 — 2** (search legacy, getTopSongs), **70 — 1** (getAvatar); остальные эндпоинты спецификации — вне поставки (см. «Явно не реализуется»).

## 4. Разбивка на поставки (соотносится с §14 основного документа)

- **F3a (ядро, JSON+XML, token+salt)**: ping, getLicense, getMusicFolders, getScanStatus, getUser(s), getArtists, getArtist, getAlbum, getSong, getIndexes, getMusicDirectory, getGenres, getAlbumList2, search3, getRandomSongs, getSongsByGenre, stream, download, getCoverArt, scrobble, star/unstar, setRating, getStarred2, getNowPlaying. → клиенты подключаются и играют музыку.
- **F3b (доводка)**: плейлисты CRUD, play-queue sync, getArtistInfo2/getSimilarSongs2/getAlbumInfo2/getTopSongs, getLyrics(+BySongId), bookmarks, internet radio CRUD, startScan, getOpenSubsonicExtensions + Bearer.

## 5. Приёмка

- Матрица ручной проверки: Symfonium, Ultrasonic, Feishin (JSON) + DSub (XML): логин → browse → стрим (3 битрейта) → скробл → звезда → плейлист → поиск → офлайн-кеш клиента (стрим/докачка).
- Контракт-тесты в `internal/apitest`: конверт (status/version), каждая группа эндпоинтов на фикстуре PG, изоляция user A/B на star/playlist/queue.
