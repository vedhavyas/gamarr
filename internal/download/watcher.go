package download

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gamarr/internal/config"
	"gamarr/internal/qbit"
)

// How many times the watcher re-attempts an import that failed for a reason
// that may clear on its own, and how long it waits between attempts. The
// watcher fires the moment progress reads complete, which is when the client
// may still be moving files, so the first attempt can lose that race. Tests
// shorten both.
var (
	importAttempts   = 20
	importRetryDelay = 30 * time.Second
)

// Watcher monitors qBittorrent for completed torrents and auto-imports them.
type Watcher struct {
	cfg        *config.Config
	mgr        *Manager
	processing sync.Map // hash → struct{} (currently being processed)
	imported   sync.Map // hash → struct{} (already imported)
	stopCh     chan struct{}
}

// NewWatcher creates a torrent completion watcher.
func NewWatcher(cfg *config.Config, mgr *Manager) *Watcher {
	return &Watcher{
		cfg:    cfg,
		mgr:    mgr,
		stopCh: make(chan struct{}),
	}
}

// Start begins watching for completed torrents.
func (w *Watcher) Start() {
	if !w.cfg.WatcherEnabled || !w.cfg.HasQBittorrent() {
		slog.Info("torrent watcher disabled")
		return
	}

	interval := time.Duration(w.cfg.WatcherIntervalS) * time.Second
	if interval < 10*time.Second {
		interval = 30 * time.Second
	}

	slog.Info("torrent watcher started", "interval", interval, "category", w.cfg.QBCategory)

	// Run immediately on start.
	w.checkCompleted()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.checkCompleted()
			case <-w.stopCh:
				slog.Info("torrent watcher stopped")
				return
			}
		}
	}()
}

// Stop signals the watcher to stop.
func (w *Watcher) Stop() {
	select {
	case <-w.stopCh:
		// Already closed.
	default:
		close(w.stopCh)
	}
}

// checkCompleted scans qBittorrent for completed torrents that haven't been imported.
func (w *Watcher) checkCompleted() {
	torrents, err := w.mgr.QB().GetTorrents(w.cfg.QBCategory)
	if err != nil {
		slog.Warn("watcher: could not read the download client, skipping this pass", "error", err)
		return
	}

	for _, t := range torrents {
		if t.Progress < 1.0 {
			continue
		}

		// Skip if already imported or currently processing.
		if _, ok := w.imported.Load(t.Hash); ok {
			continue
		}
		if _, ok := w.processing.Load(t.Hash); ok {
			continue
		}

		// Skip if we already have a tracked job for this torrent.
		if w.hasMatchingJob(t) {
			continue
		}

		w.processing.Store(t.Hash, struct{}{})
		go w.importTorrent(t)
	}
}

// hasMatchingJob checks if there's already a job tracking this torrent. It uses
// the same match as Manager.watchGameTorrent so a torrent renamed by the tracker
// (still claimed by an active job) is not double-imported.
func (w *Watcher) hasMatchingJob(t qbit.Torrent) bool {
	for _, item := range w.mgr.Jobs().Items() {
		title, _ := item.Data["title"].(string)
		infoHash, _ := item.Data["info_hash"].(string)
		if jobMatchesTorrent(infoHash, title, t.Hash, t.Name) {
			return true
		}
	}
	return false
}

// importTorrent auto-imports a completed torrent into the library.
func (w *Watcher) importTorrent(t qbit.Torrent) {
	defer w.processing.Delete(t.Hash)

	slog.Info("watcher: auto-importing completed torrent", "name", t.Name, "hash", t.Hash)

	// Detect platform from existing library items or default to PC.
	platf := "PC"
	platSlug := "pc"
	isPC := true

	// Check if there's a library item with a matching title for platform hints.
	if existing := w.mgr.Jobs().FindLibraryByTitle(t.Name, ""); existing != nil {
		platf = existing.Platform
		platSlug = existing.PlatformSlug
		isPC = existing.IsPC
	}

	// Create a job and organize.
	jobID := newJobID()
	w.mgr.Jobs().Set(jobID, map[string]interface{}{
		"status":        "organizing",
		"title":         t.Name,
		"platform":      platf,
		"platform_slug": platSlug,
		"is_pc":         isPC,
		"error":         nil,
		"detail":        "Auto-importing completed torrent...",
	})

	attempt := 0
	// Empty means the import wrote its own terminal state and nothing here may
	// overwrite it: a quarantined download has already had its files deleted, and
	// telling the user to go organize it by hand would be wrong.
	var giveUp string
	for {
		attempt++
		retryable := w.mgr.organizeWithScan(jobID, &t, platf, platSlug, isPC)

		if job, ok := w.mgr.Jobs().Get(jobID); ok {
			if status, _ := job["status"].(string); status == "completed" {
				w.imported.Store(t.Hash, struct{}{})
				slog.Info("watcher: auto-import completed", "name", t.Name, "attempts", attempt)

				// Optionally remove the torrent after import.
				if w.cfg.RemoveAfterImport {
					w.mgr.QB().DeleteTorrent(t.Hash, false)
					slog.Info("watcher: removed torrent after import", "name", t.Name)
				}

				// Send notification.
				if w.mgr.NotifyFunc != nil {
					w.mgr.NotifyFunc("", "download_complete", t.Name,
						"Auto-imported by watcher: "+t.Name+" ("+platf+")")
				}
				return
			}
		}

		if !retryable {
			break
		}
		if attempt >= importAttempts {
			giveUp = fmt.Sprintf("Gave up after %d attempts. The download is still in the client, "+
				"so organize it by hand once the files are in place.", attempt)
			break
		}

		// organizing is a status the job store rewrites to interrupted on startup.
		// Left at error, a restart during the wait leaves a row reading as retrying
		// with nothing retrying it.
		w.mgr.Jobs().UpdateMulti(jobID, map[string]interface{}{
			"status": "organizing",
			"detail": fmt.Sprintf("Waiting for the download client to publish the finished files, attempt %d of %d", attempt, importAttempts),
		})
		time.Sleep(importRetryDelay)

		// Ask the client again rather than reusing the value that just failed:
		// content_path changes when the client publishes the finished download.
		fresh, found, err := w.torrentByHash(t.Hash)
		switch {
		case err != nil:
			slog.Warn("watcher: could not re-read the torrent, trying again",
				"name", t.Name, "error", err)
		case !found:
			giveUp = "The download client no longer lists this torrent, so there is nothing left to import."
		default:
			t = fresh
		}
		if giveUp != "" {
			break
		}
	}

	// Bounded rather than disabled. The guard this replaces was right that a
	// broken import must not spin forever, but it could not tell a transient miss
	// from a permanent one and so treated every failure as permanent.
	w.imported.Store(t.Hash, struct{}{})
	if giveUp != "" {
		w.mgr.Jobs().UpdateMulti(jobID, map[string]interface{}{
			"status": "error", "detail": giveUp,
		})
	}
	slog.Warn("watcher: auto-import did not complete", "name", t.Name, "attempts", attempt)
}

// torrentByHash re-reads a torrent from the client by exact hash. The bool means
// anything only when the error is nil: a read that failed is not evidence the
// client stopped holding the torrent, and acting on it as though it were turns
// one bad request into a permanent give-up.
func (w *Watcher) torrentByHash(hash string) (qbit.Torrent, bool, error) {
	torrents, err := w.mgr.QB().GetTorrents(w.cfg.QBCategory)
	if err != nil {
		return qbit.Torrent{}, false, err
	}
	for _, t := range torrents {
		if strings.EqualFold(t.Hash, hash) {
			return t, true, nil
		}
	}
	return qbit.Torrent{}, false, nil
}
