package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/auth"
	"github.com/spa-skyson/music-hive/player/internal/db"
)

// Admin-API управления пользователями (F2.3, GitLab #21) и моделями
// эмбеддингов (F4.3, GitLab #29). Только is_admin=true; не-админам 403.

// usernameRE — правило имён: 3-32, [a-z0-9_-] (CITEXT хранит как есть,
// заглавные запрещаем явно, чтобы имена были предсказуемыми).
var usernameRE = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// adminUser решает админ-доступ: аутентифицированный is_admin.
// ok=false → ответ уже записан.
func (s *Server) adminUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	u, ok := auth.FromContext(r.Context())
	if !ok {
		// AUTH_DISABLED-режим: мидлварь не кладёт пользователя, резолвим
		// сами (как handleAuthTokens) — владелец, он админ.
		u, ok = s.Auth.RequestUser(r)
	}
	if !ok {
		writeErr(w, 401, "auth_required", "unauthorized")
		return auth.User{}, false
	}
	if !u.IsAdmin {
		writeErr(w, 403, "forbidden", "admin only")
		return auth.User{}, false
	}
	return u, true
}

// handleAdminUsersList: GET /api/admin/users
func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(w, r); !ok {
		return
	}
	users, err := s.Auth.Users.AdminListUsers()
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "users": users})
}

// handleAdminUsersCreate: POST /api/admin/users {username, password, is_admin?}
func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(w, r); !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	if !usernameRE.MatchString(req.Username) {
		writeErr(w, 400, "bad_username",
			"username: 3-32 символа, [a-z0-9_-]")
		return
	}
	if req.Password == "" {
		writeErr(w, 400, "bad_password", "password is required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, 500, "hash", err.Error())
		return
	}
	u, err := s.Auth.Users.AdminCreateUser(req.Username, hash, req.IsAdmin)
	if errors.Is(err, db.ErrUsernameTaken) {
		writeErr(w, 400, "username_taken", "имя уже занято")
		return
	}
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "user": u})
}

// handleAdminUsersPatch: PATCH /api/admin/users/{id}
// {disabled?|password?|is_admin?}; owner нельзя disable/разжаловать (400).
func (s *Server) handleAdminUsersPatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad_id", "bad user id")
		return
	}
	var req struct {
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
		IsAdmin  *bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	if req.Disabled == nil && req.Password == nil && req.IsAdmin == nil {
		writeErr(w, 400, "empty_patch", "ничего не менять нечего: disabled|password|is_admin")
		return
	}
	target, found, err := s.Auth.Users.AdminGetUser(id)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	if !found {
		writeErr(w, 404, "not_found", "user not found")
		return
	}
	// owner — необорудуемый админ (схема 00001): ни выключить, ни разжаловать
	if target.IsOwner && req.Disabled != nil && *req.Disabled {
		writeErr(w, 400, "owner_protected", "владельца нельзя отключить")
		return
	}
	if target.IsOwner && req.IsAdmin != nil && !*req.IsAdmin {
		writeErr(w, 400, "owner_protected", "владельца нельзя разжаловать")
		return
	}
	var patch auth.AdminPatch
	if req.Disabled != nil {
		patch.Disabled = req.Disabled
	}
	if req.IsAdmin != nil {
		patch.IsAdmin = req.IsAdmin
	}
	if req.Password != nil {
		if *req.Password == "" {
			writeErr(w, 400, "bad_password", "password is required")
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			writeErr(w, 500, "hash", err.Error())
			return
		}
		patch.PasswordHash = &hash
	}
	// ponytail: owner-защита проверена перед UPDATE без транзакции —
	// гонка «is_owner изменился между чтением и записью» на домашнем
	// сервере не стоит ещё одного запроса.
	u, found, err := s.Auth.Users.AdminUpdateUser(id, patch)
	if err != nil {
		writeErr(w, 500, "db", err.Error())
		return
	}
	if !found {
		writeErr(w, 404, "not_found", "user not found")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "user": u})
}

// ── Модели эмбеддингов (F4.3, GitLab #29) ─────────────────────────────
//
// Реестр (embedding_models) и векторы в динамических таблицах emb_<slug>
// живут в PG, но счётчики векторов per-model знает только worker —
// поэтому оба эндпоинта ходят через worker-jobs (kind model_status /
// model_activate, контракт ФИКСИРОВАН Python-стороной), без дубля логики.

// handleAdminModelsStatus: GET /api/admin/models — срез реестра моделей:
// enqueue мгновенного job'а model_status → poll до done (таймаут 10с) →
// result как есть ({active, models[{key,dim,active,vectors}], pending}).
func (s *Server) handleAdminModelsStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(w, r); !ok {
		return
	}
	s.ensureWorkerBeforeEnqueue()
	out, code, err := s.proxyWorker("POST", "/jobs", map[string]any{"kind": "model_status"})
	if err != nil {
		writeErr(w, 503, "worker_unreachable",
			"model registry lives in the worker — start `music-hive worker`")
		return
	}
	if code >= 400 {
		writeErr(w, code, "worker_error", "worker rejected model_status job")
		return
	}
	id, _ := out["id"].(float64)
	if id == 0 {
		writeErr(w, 502, "bad_worker_response", "worker returned no job id")
		return
	}
	jobPath := "/jobs/" + strconv.FormatInt(int64(id), 10)
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, code, err := s.proxyWorker("GET", jobPath, nil)
		if err != nil || code >= 400 {
			writeErr(w, 502, "worker_error", "polling model_status job failed")
			return
		}
		switch job["status"] {
		case "done":
			res, _ := job["result"].(map[string]any)
			if res == nil {
				writeErr(w, 502, "bad_worker_response", "model_status job has no result")
				return
			}
			writeJSON(w, res)
			return
		case "failed":
			writeErr(w, 502, "job_failed", "model_status failed: "+fmt.Sprint(job["error"]))
			return
		}
		if time.Now().After(deadline) {
			writeErr(w, 504, "worker_timeout", "model_status job not done in 10s")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// handleAdminModelsActivate: POST /api/admin/models/activate {key} —
// переключить активную модель: enqueue job model_activate {model_key}
// (worker: re-embed → activate → пересчёт вкуса; по завершении reloadKinds
// заставит player перечитать векторы). Прогресс — обычный GET /api/jobs/{id}.
func (s *Server) handleAdminModelsActivate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminUser(w, r); !ok {
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_json", "bad json")
		return
	}
	kind, name, ok := strings.Cut(req.Key, ":")
	if !ok || kind == "" || name == "" || len(req.Key) > 256 {
		writeErr(w, 400, "bad_key", "key must be <kind>:<name>, e.g. onnx:mini-lm")
		return
	}
	s.ensureWorkerBeforeEnqueue()
	var jobID int64
	local := false
	out, code, err := s.proxyWorker("POST", "/jobs", map[string]any{
		"kind":    "model_activate",
		"payload": map[string]string{"model_key": req.Key},
	})
	switch {
	case err != nil:
		// worker недоступен — как остальные jobs: fallback enqueue в БД,
		// живой worker подберёт pending-job при следующем poll-цикле.
		payload, _ := json.Marshal(map[string]string{"model_key": req.Key})
		id, e2 := s.Store.EnqueueJob("model_activate", string(payload))
		if e2 != nil {
			writeErr(w, 503, "enqueue_failed",
				"worker unreachable and local enqueue failed: "+err.Error())
			return
		}
		jobID, local = id, true
	case code >= 400:
		writeErr(w, code, "worker_error", "worker rejected model_activate job")
		return
	default:
		if id, ok := out["id"].(float64); ok {
			jobID = int64(id)
		}
	}
	if jobID == 0 {
		writeErr(w, 502, "bad_worker_response", "worker returned no job id")
		return
	}
	resp := map[string]any{
		"ok": true, "job_id": jobID, "status": "pending",
		"hint": "track via GET /api/jobs/{id}",
	}
	if local {
		resp["via"] = "local_db"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}
