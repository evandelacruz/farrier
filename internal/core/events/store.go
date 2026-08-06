package events

import "sync"

// Store holds in-flight and completed Jobs, keyed by ID, so a job created
// by one code path — a mutation verb's handler — can be found by another,
// using only the ID returned to the caller: the SSE handler behind
// `GET /jobs/{id}/events`, or a CLI command that starts a job and then
// attaches to render it.
type Store struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{jobs: make(map[string]*Job)}
}

// New creates a Job, registers it in the store under its ID, and returns
// it.
func (s *Store) New() *Job {
	j := NewJob()
	s.mu.Lock()
	s.jobs[j.id] = j
	s.mu.Unlock()
	return j
}

// Get looks up a Job by ID. ok is false if no job with that ID was ever
// created in this store.
func (s *Store) Get(id string) (job *Job, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}
