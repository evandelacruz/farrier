// Package events implements the CORE-002 job/progress event model: every
// long-running operation is a Job identified by an ID, and its progress is
// an ordered stream of Events that both the CLI and the dashboard render
// from the same data.
package events

import "time"

// State is the state of a job, or one of its steps, at the moment an Event
// was emitted.
type State string

const (
	// StateStarted marks a step — or, with an empty Step, the job as a
	// whole — beginning.
	StateStarted State = "started"
	// StateSucceeded marks a step, or the whole job, completing without
	// error. On the whole job it is terminal: no further events follow.
	StateSucceeded State = "succeeded"
	// StateFailed marks a step, or the whole job, ending in error. On the
	// whole job it is terminal: no further events follow.
	StateFailed State = "failed"
)

// Event is one point in a Job's progress stream. The five fields are the
// CORE-002 schema and are exactly what both frontends render: jobId, step,
// state, detail, timestamp.
type Event struct {
	JobID     string    `json:"jobId"`
	Step      string    `json:"step"`
	State     State     `json:"state"`
	Detail    string    `json:"detail"`
	Timestamp time.Time `json:"timestamp"`
}
