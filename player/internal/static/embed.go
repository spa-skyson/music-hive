package static

import (
	"embed"
	"io/fs"
)

// Web UI отдаётся из dist/ — Vite-сборка React-фронта (issue #48),
// вшитая в бинарник. Пока фронт не собран, в dist/ лежит плейсхолдер
// index.html («Фронтенд не собран»). Директива all: перед dist
// обязательна — в dist/ попадают dot-файлы (копии статики из public/
// и .vite/manifest.json при включённом build.manifest), которые
// обычный embed молча пропускает.
//
//go:embed all:dist
var FS embed.FS

// Root возвращает файловую систему для отдачи статики (содержимое dist/).
func Root() (fs.FS, error) {
	return fs.Sub(FS, "dist")
}
