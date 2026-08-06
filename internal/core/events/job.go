package events

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Job is one long-running operation, identified by ID, whose progress is an
// ordered Event stream. A Job is safe for concurrent use: one goroutine
// typically emits events while others — a CLI renderer, an SSE handler —
// subscribe to render them.
//
// A Job ends the moment a StateSucceeded or StateFailed event is emitted
// for it (Succeeded/Failed, or Emit called directly with either state and
// an empty step); after that its stream is closed and Emit panics if
// called again. This is CORE-002's whole-job terminal state, distinct from
// a step's own started/succeeded/failed events, which do not close
// anything.
type Job struct {
	id string

	mu       sync.Mutex
	events   []Event
	subs     map[int]*subscriber
	nextSub  int
	terminal bool
}

// NewJob creates a Job with a fresh, unique ID and no events yet.
func NewJob() *Job {
	return &Job{id: newID(), subs: make(map[int]*subscriber)}
}

// ID returns the job's identifier.
func (j *Job) ID() string {
	return j.id
}

// Emit records an event and delivers it to every current subscriber, in
// order. It panics if the job has already reached a terminal event — a
// finished job's stream cannot be reopened.
func (j *Job) Emit(step string, state State, detail string) Event {
	j.mu.Lock()
	if j.terminal {
		j.mu.Unlock()
		panic(fmt.Sprintf("events: job %s: emit after terminal event", j.id))
	}

	ev := Event{
		JobID:     j.id,
		Step:      step,
		State:     state,
		Detail:    detail,
		Timestamp: time.Now().UTC(),
	}
	j.events = append(j.events, ev)

	// Only a whole-job event — empty step — ends the stream. A step's own
	// succeeded/failed event reports that step's outcome and does not
	// close anything; the caller keeps emitting further steps, then a
	// final Succeeded/Failed call for the job as a whole.
	terminal := step == "" && isTerminal(state)
	subs := make([]*subscriber, 0, len(j.subs))
	for _, s := range j.subs {
		subs = append(subs, s)
	}
	if terminal {
		j.terminal = true
		j.subs = make(map[int]*subscriber)
	}
	j.mu.Unlock()

	for _, s := range subs {
		s.push(ev)
		if terminal {
			s.close()
		}
	}
	return ev
}

// Started emits a StateStarted event for step.
func (j *Job) Started(step, detail string) Event {
	return j.Emit(step, StateStarted, detail)
}

// Succeeded emits a job-terminal StateSucceeded event, with an empty step,
// and closes the stream.
func (j *Job) Succeeded(detail string) Event {
	return j.Emit("", StateSucceeded, detail)
}

// Failed emits a job-terminal StateFailed event, with an empty step, and
// closes the stream.
func (j *Job) Failed(detail string) Event {
	return j.Emit("", StateFailed, detail)
}

// Done reports whether the job has reached a terminal event.
func (j *Job) Done() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.terminal
}

// Events returns a snapshot of every event emitted so far, in order.
func (j *Job) Events() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Event, len(j.events))
	copy(out, j.events)
	return out
}

// Subscribe returns a channel that first replays every event already
// emitted, in order, then delivers new events as Emit is called. If the
// job has already reached its terminal event, the channel carries the full
// history and is closed once drained. Otherwise the channel closes when
// the job reaches its terminal event.
//
// This is what both frontends attach to: the CLI subscribes and prints
// each event as it arrives; the dashboard's SSE handler subscribes and
// writes each event to the response as an SSE frame. A late subscriber —
// a dashboard reconnect, a CLI attaching mid-operation — sees the same
// replay, so no event is missed regardless of when the frontend attaches.
//
// Callers that stop reading before the job finishes must call the
// returned cancel func, or the subscription — and its backing goroutine —
// leaks for the life of the Job.
func (j *Job) Subscribe() (<-chan Event, func()) {
	j.mu.Lock()

	sub := newSubscriber(j.events)
	out := make(chan Event)

	if j.terminal {
		j.mu.Unlock()
		sub.close()
		go forward(sub, out)
		return out, func() {}
	}

	id := j.nextSub
	j.nextSub++
	j.subs[id] = sub
	j.mu.Unlock()

	go forward(sub, out)

	cancel := func() {
		j.mu.Lock()
		delete(j.subs, id)
		j.mu.Unlock()
		sub.close()
	}
	return out, cancel
}

func isTerminal(state State) bool {
	return state == StateSucceeded || state == StateFailed
}

// subscriber queues events for one Subscribe call behind a mutex and a
// condition variable, so Emit can hand off events without blocking on a
// slow or stalled reader: forward, not Emit, does the (possibly blocking)
// channel send.
type subscriber struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []Event
	closed bool
}

func newSubscriber(backlog []Event) *subscriber {
	s := &subscriber{queue: append([]Event(nil), backlog...)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *subscriber) push(ev Event) {
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	s.cond.Broadcast()
	s.mu.Unlock()
}

// close marks the subscriber done. forward drains any queued events first,
// then returns. Safe to call more than once (Job.Subscribe's cancel func
// and a job-terminal event can each call it).
func (s *subscriber) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func forward(s *subscriber, out chan<- Event) {
	defer close(out)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		ev := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		out <- ev
	}
}

// newID returns a fresh, unique job identifier: 8 random bytes, hex
// encoded.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("events: generate job id: %v", err))
	}
	return hex.EncodeToString(b[:])
}
