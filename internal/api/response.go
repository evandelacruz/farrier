package api

import (
	"encoding/json"
	"net/http"
)

// writeError writes a JSON error body: {"error": "..."}.
func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// writeJobAccepted writes the API-002 mutation-verb response: 202 Accepted
// with the started job's ID, {"jobId": "..."}.
func writeJobAccepted(w http.ResponseWriter, jobID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"jobId": jobID})
}
