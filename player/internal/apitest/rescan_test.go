package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spa-skyson/music-hive/player/internal/api"
)

// rescanResp — ответ POST /api/library/rescan (и /api/jobs/{kind}).
type rescanResp struct {
	OK             bool   `json:"ok"`
	ID             int64  `json:"id"`
	JobID          int64  `json:"job_id"`
	Status         string `json:"status"`
	AlreadyRunning bool   `json:"already_running"`
	Via            string `json:"via"`
	Code           string `json:"code"`
}

func postRescan(t *testing.T, server *api.Server, body string) (*httptest.ResponseRecorder, rescanResp) {
	t.Helper()
	rec := serve(server, jsonReq("POST", "/api/library/rescan", body))
	var out rescanResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec, out
}

// TestLibraryRescanBodyRouting: full:false/пустое тело/{}/мусор-в-поле → scan,
// full:true → full_rescan (воркер недоступен → fallback в локальную БД).
func TestLibraryRescanBodyRouting(t *testing.T) {
	server, store := openPGHTTPServer(t)

	for _, tc := range []struct {
		body string
		kind string
	}{
		{"", "scan"},   // пустое тело → дефолт
		{"{}", "scan"}, // пустой объект → дефолт
		{`{"full":false}`, "scan"},
		{`{"full":"yes"}`, "scan"}, // full не-bool → снисходительно дефолт
		{`{"full":null}`, "scan"},
		{`{"unrelated":1}`, "scan"},
		{`{"full":true}`, "full_rescan"},
	} {
		// Guard от дублей активен и между кейсами: гасим джобы предыдущих.
		if _, err := store.DB.Exec(`UPDATE jobs SET status='done', updated_at=now() WHERE status IN ('pending','running')`); err != nil {
			t.Fatal(err)
		}
		rec, out := postRescan(t, server, tc.body)
		if rec.Code != 200 || !out.OK || out.ID == 0 || out.JobID != out.ID || out.AlreadyRunning {
			t.Fatalf("rescan body=%q status=%d out=%+v", tc.body, rec.Code, out)
		}
		if out.Via != "local_db" {
			t.Fatalf("body=%q: expected local_db fallback, got via=%q", tc.body, out.Via)
		}
		job, err := store.GetJob(out.ID)
		if err != nil || job == nil {
			t.Fatalf("body=%q: GetJob(%d)=%v,%v", tc.body, out.ID, job, err)
		}
		if job.Kind != tc.kind || job.Status != "pending" {
			t.Fatalf("body=%q: kind=%s status=%s, want %s/pending", tc.body, job.Kind, job.Status, tc.kind)
		}
	}
}

// TestLibraryRescanGuardWhileActive: вторая постановка того же kind при
// pending/running не создаёт джобу — already_running=true и id существующей;
// done/failed guard не считает; scan и full_rescan независимы.
func TestLibraryRescanGuardWhileActive(t *testing.T) {
	server, store := openPGHTTPServer(t)

	setStatus := func(id int64, status string) {
		t.Helper()
		if _, err := store.DB.Exec(`UPDATE jobs SET status=$2, updated_at=now() WHERE id=$1`, id, status); err != nil {
			t.Fatal(err)
		}
	}

	_, first := postRescan(t, server, "")
	if first.AlreadyRunning {
		t.Fatalf("first enqueue reported already_running: %+v", first)
	}

	// pending → дубль не ставится.
	rec, dup := postRescan(t, server, "")
	if rec.Code != 200 || !dup.OK || !dup.AlreadyRunning || dup.ID != first.ID {
		t.Fatalf("guard pending: status=%d out=%+v want id=%d", rec.Code, dup, first.ID)
	}

	// running (воркер сделал claim) → тоже дубль.
	setStatus(first.ID, "running")
	rec, dup = postRescan(t, server, "")
	if rec.Code != 200 || !dup.AlreadyRunning || dup.ID != first.ID || dup.Status != "running" {
		t.Fatalf("guard running: status=%d out=%+v want id=%d running", rec.Code, dup, first.ID)
	}

	// done → guard пропускает, ставится новая.
	setStatus(first.ID, "done")
	rec, second := postRescan(t, server, "")
	if rec.Code != 200 || second.AlreadyRunning || second.ID == first.ID {
		t.Fatalf("after done: status=%d out=%+v first=%d", rec.Code, second, first.ID)
	}

	// full_rescan — другой kind, scan-джоба ему не мешает и имеет свой guard.
	rec, full := postRescan(t, server, `{"full":true}`)
	if rec.Code != 200 || full.AlreadyRunning || full.ID == 0 {
		t.Fatalf("full rescan: status=%d out=%+v", rec.Code, full)
	}
	rec, fullDup := postRescan(t, server, `{"full":true}`)
	if rec.Code != 200 || !fullDup.AlreadyRunning || fullDup.ID != full.ID {
		t.Fatalf("full rescan guard: status=%d out=%+v want id=%d", rec.Code, fullDup, full.ID)
	}

	// Тот же guard у публичного POST /api/jobs/{kind}.
	rec = serve(server, jsonReq("POST", "/api/jobs/embed", ""))
	if rec.Code != 200 {
		t.Fatalf("jobs/embed status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = serve(server, jsonReq("POST", "/api/jobs/embed", ""))
	var dupKind rescanResp
	if err := json.Unmarshal(rec.Body.Bytes(), &dupKind); err != nil {
		t.Fatal(err)
	}
	if !dupKind.OK || !dupKind.AlreadyRunning || dupKind.ID == 0 {
		t.Fatalf("jobs/embed guard=%+v", dupKind)
	}
}

// TestLibraryRescanQuickChainsEmbed: быстрое обновление (#57) кладёт в
// payload scan-джобы маркер цепочки — воркер после scan поставит embed;
// публичный POST /api/jobs/scan — «ровно эта джоба», без маркера.
func TestLibraryRescanQuickChainsEmbed(t *testing.T) {
	server, store := openPGHTTPServer(t)

	_, quick := postRescan(t, server, `{"full":false}`)
	if quick.AlreadyRunning || quick.ID == 0 {
		t.Fatalf("quick rescan: %+v", quick)
	}
	job, err := store.GetJob(quick.ID)
	if err != nil || job == nil {
		t.Fatalf("GetJob(%d)=%v,%v", quick.ID, job, err)
	}
	if job.Kind != "scan" {
		t.Fatalf("kind=%s, want scan", job.Kind)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		t.Fatalf("payload %q: %v", job.Payload, err)
	}
	if payload["chain_embed"] != true {
		t.Fatalf("payload %q: want chain_embed=true", job.Payload)
	}

	// Публичный /api/jobs/scan маркер не ставит (гасим scan из шага 1 —
	// иначе guard вернёт его же).
	if _, err := store.DB.Exec(`UPDATE jobs SET status='done', updated_at=now() WHERE id=$1`, quick.ID); err != nil {
		t.Fatal(err)
	}
	rec := serve(server, jsonReq("POST", "/api/jobs/scan", ""))
	if rec.Code != 200 {
		t.Fatalf("jobs/scan status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out rescanResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == 0 {
		t.Fatalf("jobs/scan out=%s err=%v", rec.Body.String(), err)
	}
	raw, err := store.GetJob(out.ID)
	if err != nil || raw == nil {
		t.Fatalf("GetJob(%d)=%v,%v", out.ID, raw, err)
	}
	if raw.Payload != "" {
		t.Fatalf("jobs/scan payload=%q, want empty", raw.Payload)
	}
}

// TestLibraryRescanGuardCountsEmbed: guard быстрого обновления считает
// цепочку одним действием (#57) — активная embed-джоба (хвост предыдущей
// цепочки) блокирует новый быстрый scan («уже идёт»); full_rescan —
// отдельное тяжёлое действие, embed ему не помеха.
func TestLibraryRescanGuardCountsEmbed(t *testing.T) {
	server, store := openPGHTTPServer(t)

	embedID, err := store.EnqueueJob("embed", `{"trigger":"scan_chain"}`)
	if err != nil {
		t.Fatal(err)
	}

	// Быстрый rescan при живом embed — already_running с id embed-джобы,
	// новой scan-джобы нет.
	rec, out := postRescan(t, server, `{"full":false}`)
	if rec.Code != 200 || !out.OK || !out.AlreadyRunning || out.ID != embedID {
		t.Fatalf("guard embed: status=%d out=%+v want id=%d", rec.Code, out, embedID)
	}
	var n int
	if err := store.DB.QueryRow(
		`SELECT count(*) FROM jobs WHERE kind='scan'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("scan jobs=%d err=%v, want 0", n, err)
	}

	// full_rescan — другой kind, активный embed его не блокирует.
	rec, full := postRescan(t, server, `{"full":true}`)
	if rec.Code != 200 || full.AlreadyRunning || full.ID == 0 {
		t.Fatalf("full rescan: status=%d out=%+v", rec.Code, full)
	}

	// embed завершился → guard пропускает, быстрый scan ставится с цепочкой.
	if _, err := store.DB.Exec(`UPDATE jobs SET status='done', updated_at=now() WHERE id=$1`, embedID); err != nil {
		t.Fatal(err)
	}
	rec, again := postRescan(t, server, `{"full":false}`)
	if rec.Code != 200 || again.AlreadyRunning || again.ID == 0 {
		t.Fatalf("after embed done: status=%d out=%+v", rec.Code, again)
	}
	job, err := store.GetJob(again.ID)
	if err != nil || job == nil || job.Kind != "scan" || job.Status != "pending" {
		t.Fatalf("GetJob(%d)=%v,%v", again.ID, job, err)
	}
	if job.Payload == "" {
		t.Fatalf("quick rescan payload empty, want chain marker")
	}
}

// TestLibraryRescanBadJSON: непустое тело с невалидным JSON-синтаксисом — 400.
func TestLibraryRescanBadJSON(t *testing.T) {
	server, _ := openPGHTTPServer(t)

	for _, body := range []string{`garbage`, `{invalid`, `[1,2`} {
		rec := serve(server, jsonReq("POST", "/api/library/rescan", body))
		if rec.Code != 400 {
			t.Fatalf("body=%q status=%d body=%s", body, rec.Code, rec.Body.String())
		}
		var out rescanResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Code != "bad_json" {
			t.Fatalf("body=%q out=%s err=%v", body, rec.Body.String(), err)
		}
	}
}

// TestLibraryRescanProxyWorkerPath: живой воркер → прокси, ответ нормализуется
// (ok/job_id/already_running добавляются к ответу воркера).
func TestLibraryRescanProxyWorkerPath(t *testing.T) {
	server, _ := openPGHTTPServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"status":"pending"}`))
	}))
	defer fake.Close()
	server.Cfg.WorkerURL = fake.URL

	rec, out := postRescan(t, server, "")
	if rec.Code != 200 || !out.OK || out.ID != 7 || out.JobID != 7 || out.AlreadyRunning {
		t.Fatalf("proxy rescan status=%d out=%+v", rec.Code, out)
	}
}
