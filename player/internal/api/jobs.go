package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/spa-skyson/music-hive/player/internal/db"
)

func (s *Server) handleLibraryRescan(w http.ResponseWriter, r *http.Request) {
	full, err := parseRescanBody(r)
	if err != nil {
		writeErr(w, 400, "bad_json", "invalid JSON body")
		return
	}
	if full {
		s.enqueueRescanJob(w, "full_rescan", "")
		return
	}
	// #57: быстрое обновление — одно действие «scan → embed»: маркер
	// цепочки в payload джобы; воркер после успешного scan ставит embed.
	// Guard (#57) считает цепочку одним действием: активный embed — хвост
	// предыдущей цепочки — тоже значит «обновление уже идёт».
	s.enqueueRescanJob(w, "scan", `{"chain_embed":true}`, "embed")
}

// parseRescanBody снисходительно читает тело {"full": bool} (#56):
// пустое тело / `{}` / не-объект / full не-bool → false (быстрый scan);
// невалидный JSON-синтаксис при непустом теле — ошибка (400).
func parseRescanBody(r *http.Request) (bool, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return false, nil
	}
	full, _ := obj["full"].(bool)
	return full, nil
}

// enqueueRescanJob ставит джобу kind с payloadJSON ("" — без payload):
// сначала через воркер (POST /jobs), при его недоступности — локально в БД
// (fallback). Guard от дублей (#56): если джоба kind — или один из
// guardKinds (#57: активный embed при быстром обновлении — хвост цепочки
// scan→embed) — уже pending/running, вторую не ставим, отвечаем
// {"ok":true,"id":<существующей>,"already_running":true}.
func (s *Server) enqueueRescanJob(w http.ResponseWriter, kind, payloadJSON string, guardKinds ...string) {
	s.ensureWorkerBeforeEnqueue()
	for _, g := range append([]string{kind}, guardKinds...) {
		if active, err := s.Store.FindActiveJob(g); err != nil {
			writeErr(w, 500, "db", err.Error())
			return
		} else if active != nil {
			writeJSON(w, map[string]any{
				"ok": true, "id": active.ID, "job_id": active.ID,
				"status": active.Status, "already_running": true,
			})
			return
		}
	}
	body := map[string]any{"kind": kind}
	if payloadJSON != "" {
		var payload any
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			writeErr(w, 500, "bad_payload", err.Error())
			return
		}
		body["payload"] = payload
	}
	out, code, err := s.proxyWorker("POST", "/jobs", body)
	if err != nil {
		id, e2 := s.Store.EnqueueJob(kind, payloadJSON)
		if e2 != nil {
			writeErr(w, 503, "enqueue_failed", "worker unreachable and local enqueue failed: "+err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"ok": true, "id": id, "job_id": id, "status": "pending",
			"via": "local_db", "already_running": false,
			"hint": "start `music-hive worker` to process jobs",
		})
		return
	}
	if code >= 400 {
		log.Printf("worker enqueue %s status %d", kind, code)
		writeErr(w, code, "worker_error", "worker error")
		return
	}
	if out == nil {
		out = map[string]any{}
	}
	out["ok"] = true
	out["already_running"] = false
	if id, ok := out["id"].(float64); ok {
		out["job_id"] = int64(id)
	}
	writeJSON(w, out)
}

func (s *Server) handleEnqueueJob(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind == "" {
		writeErr(w, 400, "kind_required", "kind required")
		return
	}
	// Публичный endpoint — «ровно эта джоба», без chain-маркера (#57):
	// цепочку быстрого обновления собирает только /api/library/rescan.
	s.enqueueRescanJob(w, kind, "")
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	jobs, err := s.Store.ListJobs(status, limit)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	if jobs == nil {
		jobs = []db.Job{}
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobPublic(j))
	}
	writeJSON(w, map[string]any{"jobs": out, "count": len(out)})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad_id", "bad id")
		return
	}
	out, code, err := s.proxyWorker("GET", "/jobs/"+strconv.FormatInt(id, 10), nil)
	if err == nil && code < 400 && out != nil {
		if res, ok := out["result"].(map[string]any); ok {
			if prog, ok := res["progress"]; ok {
				out["progress"] = prog
			}
		}
		writeJSON(w, out)
		return
	}
	j, err := s.Store.GetJob(id)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	if j == nil {
		writeErr(w, 404, "not_found", "not found")
		return
	}
	writeJSON(w, jobPublic(*j))
}

func jobPublic(j db.Job) map[string]any {
	m := map[string]any{
		"id": j.ID, "kind": j.Kind, "status": j.Status,
		"error": j.Error, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt,
	}
	if j.Result != "" {
		var parsed any
		if json.Unmarshal([]byte(j.Result), &parsed) == nil {
			m["result"] = parsed
			if pm, ok := parsed.(map[string]any); ok {
				if prog, ok := pm["progress"]; ok {
					m["progress"] = prog
				}
			}
		} else {
			m["result_json"] = j.Result
		}
	}
	return m
}

func (s *Server) proxyWorker(method, path string, body any) (map[string]any, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.Cfg.WorkerURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out, res.StatusCode, nil
}
