package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/backup"
)

// snapshotsResponse is GET /snapshots's body: the destination's backup
// history, newest first, with camelCase field names matching the
// convention every other verb in this package uses.
type snapshotsResponse struct {
	Snapshots []snapshotResponse `json:"snapshots"`
}

type snapshotResponse struct {
	Key       string    `json:"key"`
	SizeBytes int64     `json:"sizeBytes"`
	Modified  time.Time `json:"modified"`
	Age       string    `json:"age"`
}

func newSnapshotsResponse(snapshots []backup.Snapshot) snapshotsResponse {
	out := snapshotsResponse{Snapshots: make([]snapshotResponse, len(snapshots))}
	for i, s := range snapshots {
		out.Snapshots[i] = snapshotResponse{
			Key:       s.Key,
			SizeBytes: s.SizeBytes,
			Modified:  s.Modified,
			Age:       s.Age.Round(time.Second).String(),
		}
	}
	return out
}

// handleSnapshots implements GET /snapshots (API-001, UI-002): like
// GET /status and unlike every mutation verb, it is a read — it opens the
// destination named by the required `to` query parameter and calls
// backup.History synchronously, returning the list directly instead of a
// job ID.
//
// This is the one verb with no CLI counterpart, and deliberately so:
// spec.md "Interfaces" assigns backup history to the dashboard, and its
// CLI table covers health and replication lag under `status`. The logic
// still lives in core (backup.History) rather than here, so a `snapshots`
// command — if Evan ever wants one — is a flag-parsing shim over the same
// function, not a reimplementation.
//
// `to` is the same destination URI `backup -to` writes to and `status -to`
// measures lag against (BKUP-005). It is required here, because unlike
// status — which has a whole report to give with lag left unmeasured —
// backup history with no destination is not a degraded answer, it is no
// answer at all.
//
// The endpoint reaches no host: a destination is object storage or a
// filesystem path the control plane can already see, so backup history
// stays readable when the forge host is down — which is exactly when an
// operator is looking for it (spec.md "the control plane runs on the
// operator's machine, outside the failure domain").
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	if strings.TrimSpace(to) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("to is required"))
		return
	}

	dest, err := s.openDestination(to)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("to: %w", err))
		return
	}

	snapshots, err := s.snapshotHistory(r.Context(), dest, time.Now())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newSnapshotsResponse(snapshots))
}
