// Package web serves the live dashboard: a snapshot endpoint for first load
// and a Server-Sent Events stream for everything after.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/jwil007/roamjev/internal/agent"
)

//go:embed static
var static embed.FS

func Handler(store *agent.Store) http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.Snapshot())
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, cancel := store.Subscribe()
		defer cancel()
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				fl.Flush()
			case e := <-ch:
				b, err := json.Marshal(e)
				if err != nil {
					continue
				}
				_, _ = w.Write([]byte("data: "))
				_, _ = w.Write(b)
				_, _ = w.Write([]byte("\n\n"))
				fl.Flush()
			}
		}
	})
	return mux
}

func Serve(addr string, store *agent.Store) *http.Server {
	srv := &http.Server{Addr: addr, Handler: Handler(store),
		ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("dashboard server", "err", err)
		}
	}()
	return srv
}
