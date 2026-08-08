package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleJobEvents implements GET /jobs/{id}/events (API-002): it streams
// the job's CORE-002 event schema — {jobId, step, state, detail,
// timestamp} — over SSE, one "data:" frame per event. Job.Subscribe
// replays every event already emitted before delivering new ones, so a
// client that attaches after the job started still sees the full history;
// the stream closes when the job reaches its terminal event or the client
// disconnects.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("job %q not found", r.PathValue("id")))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub, cancel := job.Subscribe()
	defer cancel()

	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
