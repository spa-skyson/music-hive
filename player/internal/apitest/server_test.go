package apitest

import (
	"bytes"
	"net/http"
	"net/http/httptest"

	"github.com/spa-skyson/music-hive/player/internal/api"
)

// serve/jsonReq/flush — общие хелперы apitest-пакета (PG-тесты).
func serve(server *api.Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func jsonReq(method, path, body string) *http.Request {
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func flush(server *api.Server) {
	if server.Play != nil && server.Play.Flush != nil {
		server.Play.Flush()
	}
}
