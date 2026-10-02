// Системные эндпоинты Subsonic — производны от Navidrome
// (https://github.com/navidrome/navidrome, server/subsonic/{system,opensubsonic}.go),
// © 2016—2026 Navidrome contributors, GPL-3.0.

package subsonic

import (
	"net/http"

	"github.com/spa-skyson/music-hive/player/internal/auth"
)

// ping — проба аутентификации: конверт ok.
func (rt *Router) ping(_ *http.Request) (*Subsonic, error) {
	return rt.NewResponse(), nil
}

// getLicense — лицензия валидна всегда (свой сервер).
func (rt *Router) getLicense(r *http.Request) (*Subsonic, error) {
	resp := rt.NewResponse()
	license := &License{Valid: true}
	// email — вошедший пользователь (у пользователей нет адресов, поле
	// необязательное; часть клиентов показывает его в «о сервере»).
	if u, ok := auth.FromContext(r.Context()); ok {
		license.Email = u.Username
	}
	resp.License = license
	return resp, nil
}

// getOpenSubsonicExtensions — публичный (без auth) список реализованных
// расширений OpenSubsonic. Честно только то, что есть: formPost —
// POST-тела мержатся в query (postFormToQueryParams).
func (rt *Router) getOpenSubsonicExtensions(_ *http.Request) (*Subsonic, error) {
	resp := rt.NewResponse()
	resp.OpenSubsonicExtensions = &OpenSubsonicExtensions{
		{Name: "formPost", Versions: []int32{1}},
	}
	return resp, nil
}
