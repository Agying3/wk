package lifecycle

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
)

// ListenControl never removes an existing socket: an operator must first prove
// its owner has exited. The parent directory must be private to the service.
func ListenControl(path string, handler http.Handler) (*http.Server, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	s := &http.Server{Handler: handler, ReadHeaderTimeout: 5e9, MaxHeaderBytes: 8192}
	go func() { _ = s.Serve(l) }()
	return s, nil
}

// Control is available only on the private Unix socket, not public HTTP routes.
func Control(release string, foreground *Gate, ready func() bool, finalize func() error, stop func()) http.Handler {
	return ControlWithOptions(release, foreground, ready, finalize, stop, ControlOptions{})
}

// ControlOptions enables opt-in overlap controls. Callbacks must acquire their
// shared ownership before returning; a failed promotion keeps dispatch paused.
type ControlOptions struct {
	Status             func() map[string]any
	ActivateBackground func() error
	ActivateManagement func() error
}

func ControlWithOptions(release string, foreground *Gate, ready func() bool, finalize func() error, stop func(), options ControlOptions) http.Handler {
	m := http.NewServeMux()
	var mu sync.Mutex
	stopping := false
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		p, n := foreground.Status()
		b, tasks := Default.Status()
		status := map[string]any{}
		if options.Status != nil {
			for key, value := range options.Status() {
				status[key] = value
			}
		}
		status["release"], status["paused"], status["active"] = release, p, n
		status["background_paused"], status["background"], status["ready"] = b, tasks, ready()
		_ = json.NewEncoder(w).Encode(status)
	})
	m.HandleFunc("POST /{action}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if stopping {
			http.Error(w, "already stopping", 409)
			return
		}
		if r.URL.Query().Get("release") != release {
			http.Error(w, "release mismatch", 409)
			return
		}
		switch r.PathValue("action") {
		case "pause-background", "activate-background", "activate-management":
			var status map[string]any
			if options.Status != nil {
				status = options.Status()
			}
			if status["mode"] != "overlap" || status["overlap_protocol"] != 1 {
				http.Error(w, "overlap protocol unavailable", http.StatusConflict)
				return
			}
			switch r.PathValue("action") {
			case "pause-background":
				Default.Pause()
			case "activate-background":
				if options.ActivateBackground == nil {
					http.Error(w, "background promotion unavailable", http.StatusConflict)
					return
				}
				if err := options.ActivateBackground(); err != nil {
					http.Error(w, "background promotion failed", http.StatusConflict)
					return
				}
				Default.Resume()
			case "activate-management":
				if options.ActivateManagement == nil {
					http.Error(w, "management promotion unavailable", http.StatusConflict)
					return
				}
				if err := options.ActivateManagement(); err != nil {
					http.Error(w, "management promotion failed", http.StatusConflict)
					return
				}
			}
		case "pause":
			foreground.Pause()
		case "quiesce":
			p, n := foreground.Status()
			if !p || n != 0 {
				http.Error(w, "foreground not drained", 409)
				return
			}
			Default.Pause()
		case "resume":
			var status map[string]any
			if options.Status != nil {
				status = options.Status()
			}
			if status["mode"] != "overlap" || status["background_owner"] == true {
				Default.Resume()
			}
			foreground.Resume()
		case "stop":
			p, n := foreground.Status()
			b, tasks := Default.Status()
			if !p || !b || n != 0 || tasks != 0 {
				http.Error(w, "not drained", 409)
				return
			}
			if err := finalize(); err != nil {
				http.Error(w, "final commit failed; release remains paused", 500)
				return
			}
			stopping = true
			// The process may exit before net/http writes a chunked terminator.
			// A fixed-length flushed acknowledgment remains complete on close.
			w.Header().Set("Content-Length", "3")
			fmt.Fprintln(w, "ok")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			stop()
			return
		default:
			http.Error(w, "unknown action", 404)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return m
}
