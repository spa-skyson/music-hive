// TestSearchStubsNotImplemented — legacy search остаётся 501 (старейшая
// форма параметров); search2/search3 реализованы (#25) и без Catalog
// (юнит-режим) тоже отвечают 501, не паникой.

package subsonic

import (
	"net/http"
	"strings"
	"testing"
)

// asMap — локальная копия для внутреннего test-пакета (внешний
// catalog_pg_test имеет свою: пакеты тестов не видят файлы друг друга).
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

func TestSearchStubsNotImplemented(t *testing.T) {
	rt, _ := newTestRouter(t, 5)
	for _, path := range []string{"search", "search2", "search3"} {
		for _, spelling := range []string{path, path + ".view"} {
			rec := do(rt, "GET", "/"+spelling+"?u=owner&p="+testPassword+"&v=1.16.1&c=test&f=json&query=anything")
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("%s: status=%d body=%s", spelling, rec.Code, rec.Body)
			}
			sr := subResp(t, rec)
			errObj := asMap(t, sr["error"])
			if sr["status"] != "failed" || errObj["code"].(float64) != 0 ||
				!strings.Contains(errObj["message"].(string), "not implemented") {
				t.Fatalf("%s: %v", spelling, sr)
			}
		}
	}
}
