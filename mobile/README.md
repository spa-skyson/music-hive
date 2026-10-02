# Flutter client (music_hive_app)

План паритета с вебом: **[docs/MOBILE.md](../docs/MOBILE.md)** §B.

## Сейчас (Phase 1–2)

Рабочий клиент к Go player:
- **Главная** — радио, share, миксы, артисты/альбомы/треки
- **Сейчас** — artwork, seek, skip, like/dislike, очередь/плейлист
- **Библиотека** — треки/артисты/альбомы + поиск
- **Профиль** — health, вкус, share-ссылки
- mini-player над табами, `just_audio` (+ media_kit на Linux)

```bash
cd mobile/flutter
flutter pub get
flutter run -d linux
```

Логин:
- для LAN укажи `http://<server-lan-ip>:8787` и `MUSIC_HIVE_API_TOKEN` сервера;
- URL и token сохраняются локально через `SharedPreferences`;
- public build не содержит URL, паролей или токенов;
- private build может использовать `--dart-define=MUSIC_HIVE_BASE_URL=...`
  и `--dart-define=MUSIC_HIVE_API_TOKEN=...` (значения попадут в APK).

Player (LAN):
```bash
export MUSIC_HIVE_ROOT=$PWD MUSIC_HIVE_DB_PATH=$PWD/data/db/music-hive.db
set -a && source .env && set +a
./player/bin/music-hive-player   # MUSIC_HIVE_PLAYER_ADDR=0.0.0.0:8787
```

После обновления кода: **`R`** (hot restart) в `flutter run`.
