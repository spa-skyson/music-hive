package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// F4.3 (GitLab #29): admin-эндпоинты жизненного цикла моделей эмбеддингов.
// Живой worker подменён httptest-моком с фиксированным контрактом
// (model_status — мгновенный result; model_activate — pending-job).

// mockWorkerLife — минимальный worker-сервер: POST /jobs {kind,payload} →
// 201 {id,status}; GET /jobs/{id} → job. model_status сразу done с
// фиксированным result (первый poll отдаёт running — проверяем цикл
// опроса), model_activate остаётся pending (реальный — долгий).
type mockWorkerLife struct {
	*httptest.Server
	mu        sync.Mutex
	nextID    int64
	jobs      map[int64]map[string]any
	polls     map[int64]int
	activated []map[string]any // полученные body model_activate
}

func newMockWorkerLife(t *testing.T) *mockWorkerLife {
	t.Helper()
	m := &mockWorkerLife{jobs: map[int64]map[string]any{}, polls: map[int64]int{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/jobs":
			var body struct {
				Kind    string            `json:"kind"`
				Payload map[string]string `json:"payload"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.nextID++
			id := m.nextID
			job := map[string]any{"id": id, "kind": body.Kind, "status": "pending", "payload": body.Payload}
			if body.Kind == "model_status" {
				job["status"] = "done"
				job["result"] = map[string]any{
					"active": "onnx:toy",
					"models": []map[string]any{
						{"key": "onnx:toy", "dim": 64, "active": true, "vectors": 3},
						{"key": "clap:laion/clap", "dim": 512, "active": false, "vectors": 0},
					},
					"pending": 0,
				}
			}
			if body.Kind == "model_activate" {
				m.activated = append(m.activated,
					map[string]any{"kind": body.Kind, "payload": body.Payload})
			}
			m.jobs[id] = job
			m.mu.Unlock()
			writeMockJSON(w, 201, map[string]any{"id": id, "status": job["status"]})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/jobs/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/jobs/"), 10, 64)
			m.mu.Lock()
			job := m.jobs[id]
			m.polls[id]++
			first := m.polls[id] == 1
			m.mu.Unlock()
			if job == nil {
				writeMockJSON(w, 404, map[string]any{"error": "not found"})
				return
			}
			pub := map[string]any{"id": job["id"], "kind": job["kind"],
				"status": job["status"], "result": job["result"]}
			if job["kind"] == "model_status" && first {
				pub["status"] = "running"
			}
			writeMockJSON(w, 200, pub)
		default:
			writeMockJSON(w, 404, map[string]any{"error": "not found"})
		}
	}))
	t.Cleanup(m.Server.Close)
	return m
}

func writeMockJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// TestPGAdminModelsViaWorker: статус — enqueue model_status → poll →
// result как есть; активация — 202 {job_id}, воркер получил payload;
// невалидный key → 400; не-админ → 403.
func TestPGAdminModelsViaWorker(t *testing.T) {
	worker := newMockWorkerLife(t)
	server := openPGServer(t, "s3cret", worker.URL)
	admin := loginCookie(t, server, "owner", "s3cret")

	rec := serveWith(server, "GET", "/api/admin/models", "", admin)
	if rec.Code != 200 {
		t.Fatalf("models status = %d body=%s", rec.Code, rec.Body.String())
	}
	var status struct {
		Active string `json:"active"`
		Models []struct {
			Key     string `json:"key"`
			Dim     int    `json:"dim"`
			Active  bool   `json:"active"`
			Vectors int    `json:"vectors"`
		} `json:"models"`
		Pending int `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Active != "onnx:toy" || len(status.Models) != 2 ||
		status.Models[0].Key != "onnx:toy" || status.Models[0].Dim != 64 ||
		!status.Models[0].Active || status.Models[0].Vectors != 3 ||
		status.Models[1].Active || status.Pending != 0 {
		t.Fatalf("models status = %s", rec.Body.String())
	}

	rec = serveWith(server, "POST", "/api/admin/models/activate", `{"key":"onnx:mini-lm"}`, admin)
	if rec.Code != 202 {
		t.Fatalf("activate = %d body=%s", rec.Code, rec.Body.String())
	}
	var act struct {
		OK    bool  `json:"ok"`
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil || !act.OK || act.JobID == 0 {
		t.Fatalf("activate body = %s (%v)", rec.Body.String(), err)
	}
	worker.mu.Lock()
	activated := append([]map[string]any(nil), worker.activated...)
	worker.mu.Unlock()
	if len(activated) != 1 || activated[0]["kind"] != "model_activate" {
		t.Fatalf("worker activate requests = %#v", activated)
	}
	if payload, _ := activated[0]["payload"].(map[string]string); payload["model_key"] != "onnx:mini-lm" {
		t.Fatalf("activate payload = %#v", activated[0]["payload"])
	}

	for _, bad := range []string{
		`{"key":""}`, `{"key":"onnx"}`, `{"key":":name"}`, `{"key":"onnx:"}`, `{}`, `not json`,
	} {
		if rec := serveWith(server, "POST", "/api/admin/models/activate", bad, admin); rec.Code != 400 {
			t.Fatalf("activate %s = %d, want 400", bad, rec.Code)
		}
	}

	bob := createBobViaAdmin(t, server, admin, "bob-pass-1")
	if rec := serveWith(server, "GET", "/api/admin/models", "", bob); rec.Code != 403 {
		t.Fatalf("bob models = %d, want 403", rec.Code)
	}
	if rec := serveWith(server, "POST", "/api/admin/models/activate", `{"key":"onnx:x"}`, bob); rec.Code != 403 {
		t.Fatalf("bob activate = %d, want 403", rec.Code)
	}
}

// TestPGAdminModelsWorkerDown: статус без воркера → 503 (реестр живёт в
// воркере); активация — fallback enqueue в БД (как другие jobs) → 202,
// job виден обычным GET /api/jobs/{id}.
func TestPGAdminModelsWorkerDown(t *testing.T) {
	server := openPGServer(t, "s3cret") // воркер 127.0.0.1:1 недоступен
	admin := loginCookie(t, server, "owner", "s3cret")

	if rec := serveWith(server, "GET", "/api/admin/models", "", admin); rec.Code != 503 {
		t.Fatalf("models status without worker = %d body=%s", rec.Code, rec.Body.String())
	}

	rec := serveWith(server, "POST", "/api/admin/models/activate", `{"key":"onnx:toy"}`, admin)
	if rec.Code != 202 {
		t.Fatalf("activate = %d body=%s", rec.Code, rec.Body.String())
	}
	var act struct {
		JobID int64  `json:"job_id"`
		Via   string `json:"via"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil || act.JobID == 0 || act.Via != "local_db" {
		t.Fatalf("activate body = %s (%v)", rec.Body.String(), err)
	}
	rec = serveWith(server, "GET", "/api/jobs/"+strconv.FormatInt(act.JobID, 10), "", admin)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"kind":"model_activate"`) {
		t.Fatalf("job get = %d body=%s", rec.Code, rec.Body.String())
	}
}
