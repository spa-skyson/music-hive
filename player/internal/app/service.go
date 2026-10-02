// Package app coordinates application lifecycle concerns that span domain packages.
package app

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spa-skyson/music-hive/player/internal/config"
	"github.com/spa-skyson/music-hive/player/internal/db"
	"github.com/spa-skyson/music-hive/player/internal/index"
	"github.com/spa-skyson/music-hive/player/internal/playback"
)

var reloadKinds = map[string]bool{
	"embed": true, "full_rescan": true, "clusters": true,
	"daily": true, "album_tips": true, "mix_pack": true,
	"model_activate": true, // F4.3: векторы переключились на новую модель
}

type Service struct {
	Cfg   config.Config
	Store db.Backend
	Idx   index.Searcher
	Play  *playback.Engine
	HTTP  *http.Client
}

func New(cfg config.Config, store db.Backend, idx index.Searcher, play *playback.Engine, client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{Cfg: cfg, Store: store, Idx: idx, Play: play, HTTP: client}
}

func (s *Service) Reload() error {
	rows, err := s.Store.LoadReadyTracks()
	if err != nil {
		return err
	}
	if err := s.Idx.Load(rows); err != nil {
		return err
	}
	// Вкусовые профили пер-юзерные (F2.2): сбрасываем кэш, каждый профиль
	// лениво перечитается из Store при первом обращении (размерность могла
	// измениться — резон заново свериться с БД).
	s.Play.ResetTastes()
	log.Printf("reloaded index n=%d dim=%d", s.Idx.Size(), s.Idx.Dim())
	s.Play.ReloadTransitions()
	return nil
}

func (s *Service) WorkerHealthy() bool {
	res, err := s.HTTP.Get(strings.TrimRight(s.Cfg.WorkerURL, "/") + "/jobs")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode < 500
}

func (s *Service) EnsureWorker() {
	if !s.Cfg.WorkerAutostart {
		return
	}
	if s.WorkerHealthy() {
		log.Printf("worker already up at %s", s.Cfg.WorkerURL)
		return
	}
	dataDir := filepath.Dir(filepath.Dir(s.Cfg.DBPath))
	projectRoot := filepath.Dir(dataDir)
	if root := os.Getenv("MUSIC_HIVE_ROOT"); root != "" {
		projectRoot = root
	}
	python := findPython()
	if python == "" {
		venvPython := filepath.Join(projectRoot, ".venv", "bin", "python")
		if _, err := os.Stat(venvPython); err == nil {
			python = venvPython
		}
	}
	if python == "" {
		log.Printf("worker autostart: no python found; start `music-hive worker` manually")
		return
	}
	musicHiveBin := filepath.Join(filepath.Dir(python), "music-hive")
	var cmd *exec.Cmd
	if _, err := os.Stat(musicHiveBin); err == nil {
		cmd = exec.Command(musicHiveBin, "worker")
	} else {
		cmd = exec.Command(python, "-m", "music_hive", "worker")
	}
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(),
		"MUSIC_HIVE_ROOT="+projectRoot,
		"MUSIC_HIVE_LIBRARY="+s.Cfg.Library,
		"MUSIC_HIVE_PLAYER_RELOAD_URL=http://127.0.0.1"+normalizeAddr(s.Cfg.Addr)+"/api/reload",
	)
	logPath := filepath.Join(dataDir, "worker.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		defer file.Close()
		cmd.Stdout = file
		cmd.Stderr = file
	}
	if err := cmd.Start(); err != nil {
		log.Printf("worker autostart failed: %v", err)
		return
	}
	log.Printf("worker autostarted pid=%d log=%s", cmd.Process.Pid, logPath)
	go func() { _ = cmd.Wait() }()
	for range 20 {
		time.Sleep(250 * time.Millisecond)
		if s.WorkerHealthy() {
			log.Printf("worker ready at %s", s.Cfg.WorkerURL)
			return
		}
	}
	log.Printf("worker autostart: still not healthy at %s (check %s)", s.Cfg.WorkerURL, logPath)
}

func (s *Service) WatchJobs() {
	after := time.Now().UTC().Format(time.RFC3339Nano)
	ticker := time.NewTicker(3 * time.Second)
	go func() {
		for range ticker.C {
			jobs, err := s.Store.ListDoneJobsAfter(after, 50)
			if err != nil || len(jobs) == 0 {
				continue
			}
			needReload := false
			for _, job := range jobs {
				after = job.UpdatedAt
				if reloadKinds[job.Kind] {
					needReload = true
					log.Printf("job #%d kind=%s done → reload index", job.ID, job.Kind)
				}
			}
			if needReload {
				if err := s.Reload(); err != nil {
					log.Printf("auto-reload after job: %v", err)
				}
			}
		}
	}()
}

// StartReloadProbe запускает периодический probe (GitLab #47): раз в
// Cfg.ReloadProbeSec сверяет count готовых треков в БД с размером индекса.
// Лечит «пустой каталог на время длинной job»: worker коммитит готовые
// треки инкрементально (scan→embed в full_rescan идёт сутки), а прежние
// пути reload (POST /api/reload, WatchJobs) срабатывали только по
// завершении всей job. Уровень <= 0 — выключено.
func (s *Service) StartReloadProbe() {
	if s.Cfg.ReloadProbeSec <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(s.Cfg.ReloadProbeSec) * time.Second)
	go func() {
		for range ticker.C {
			if _, err := s.ProbeOnce(); err != nil {
				log.Printf("reload probe: %v", err)
			}
		}
	}()
}

// ProbeOnce — один шаг probe: ReadyTrackCount() из БД против s.Idx.Size().
// Расхождение в любую сторону (треки и добавились, и деактивировались) —
// тот же Reload(), что дёргают WatchJobs и notify. Возвращает true, если
// перезагрузил. Ошибка count не трактуется как расхождение.
func (s *Service) ProbeOnce() (bool, error) {
	n, err := s.Store.ReadyTrackCount()
	if err != nil {
		return false, err
	}
	if n == s.Idx.Size() {
		return false, nil
	}
	log.Printf("reload probe: ready=%d index=%d → reload", n, s.Idx.Size())
	if err := s.Reload(); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return ":" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

func findPython() string {
	candidates := []string{os.Getenv("MUSIC_HIVE_PYTHON")}
	if root := os.Getenv("MUSIC_HIVE_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, ".venv", "bin", "python"))
	}
	cwd, _ := os.Getwd()
	candidates = append(candidates,
		filepath.Join(cwd, ".venv", "bin", "python"),
		filepath.Join(cwd, "..", ".venv", "bin", "python"),
		".venv/bin/python", "python3", "python",
	)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
		if _, err := os.Stat(candidate); err == nil {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
			return candidate
		}
	}
	return ""
}
