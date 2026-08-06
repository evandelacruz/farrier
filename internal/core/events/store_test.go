package events

import "testing"

func TestStoreNewAndGet(t *testing.T) {
	s := NewStore()
	j := s.New()

	got, ok := s.Get(j.ID())
	if !ok {
		t.Fatalf("Get(%q) not found after New", j.ID())
	}
	if got != j {
		t.Fatalf("Get(%q) returned a different *Job", j.ID())
	}
}

func TestStoreGetUnknownID(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("does-not-exist"); ok {
		t.Fatal("Get on unknown ID returned ok = true")
	}
}

func TestStoreJobsAreIndependentlyKeyed(t *testing.T) {
	s := NewStore()
	a := s.New()
	b := s.New()
	if a.ID() == b.ID() {
		t.Fatalf("two jobs from the same store got the same ID: %q", a.ID())
	}

	a.Succeeded("done")
	gotB, ok := s.Get(b.ID())
	if !ok || gotB.Done() {
		t.Fatalf("job b affected by job a's completion: found=%v done=%v", ok, gotB != nil && gotB.Done())
	}
}
