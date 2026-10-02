package api

// EnsureWorker preserves the server lifecycle API while delegating process
// management to the application layer.
func (s *Server) EnsureWorker() {
	s.App.EnsureWorker()
}

// WatchJobs preserves the server lifecycle API while delegating polling to the
// application layer.
func (s *Server) WatchJobs() {
	s.App.WatchJobs()
}

// StartReloadProbe preserves the server lifecycle API while delegating the
// periodic index-reload probe to the application layer (#47).
func (s *Server) StartReloadProbe() {
	s.App.StartReloadProbe()
}

func (s *Server) ensureWorkerBeforeEnqueue() {
	if !s.App.WorkerHealthy() {
		s.App.EnsureWorker()
	}
}
