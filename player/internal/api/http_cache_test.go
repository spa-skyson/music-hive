package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestStaticCacheControl(t *testing.T) {
	static := fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("index")},
		"app.js":                 &fstest.MapFile{Data: []byte("app")},
		"style.css":              &fstest.MapFile{Data: []byte("style")},
		"assets/index-a1b2c3.js": &fstest.MapFile{Data: []byte("bundle")},
	}
	s := &Server{Static: http.FS(fs.FS(static))}

	for _, tc := range []struct {
		path, want string
	}{
		{"/", "no-cache"},
		{"/app.js", "public, max-age=3600"},
		{"/style.css", "public, max-age=3600"},
		{"/assets/index-a1b2c3.js", "public, max-age=31536000, immutable"},
	} {
		rec := httptest.NewRecorder()
		s.staticHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("%s Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestAPICacheControl(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		path, want string
	}{
		{"/api/profile", "no-store"},
		{"/api/artwork/42", "private, max-age=3600"},
	} {
		rec := httptest.NewRecorder()
		withAPICacheControl(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("%s Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
}
