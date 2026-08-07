package dns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestPrintDriverSetEmitsExactRecordAndSucceeds(t *testing.T) {
	job := events.NewJob()
	d := NewPrint(job)

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	events := job.Events()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Step != StepDNSChange {
		t.Errorf("step = %q, want %q", ev.Step, StepDNSChange)
	}
	if ev.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", ev.State)
	}
	for _, want := range []string{"app.example.com", "60", "IN", "A", "203.0.113.10"} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", ev.Detail, want)
		}
	}
}

func TestPrintDriverSetInfersRecordType(t *testing.T) {
	cases := map[string]string{
		"203.0.113.10":        "A",
		"2001:db8::1":         "AAAA",
		"standby.example.com": "CNAME",
	}
	for value, wantType := range cases {
		job := events.NewJob()
		d := NewPrint(job)
		if err := d.Set(context.Background(), "app.example.com", value, 60*time.Second); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
		detail := job.Events()[0].Detail
		if !strings.Contains(detail, wantType) {
			t.Errorf("Set(%q) detail = %q, want it to contain type %q", value, detail, wantType)
		}
	}
}

func TestPrintDriverDeleteEmitsRecordAndSucceeds(t *testing.T) {
	job := events.NewJob()
	d := NewPrint(job)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	events := job.Events()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Step != StepDNSChange {
		t.Errorf("step = %q, want %q", ev.Step, StepDNSChange)
	}
	if ev.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", ev.State)
	}
	if !strings.Contains(ev.Detail, "app.example.com") {
		t.Errorf("detail = %q, want it to contain the record name", ev.Detail)
	}
}

func TestPrintDriverSetValidatesArgsBeforeEmitting(t *testing.T) {
	job := events.NewJob()
	d := NewPrint(job)

	if err := d.Set(context.Background(), "", "203.0.113.10", 60*time.Second); err == nil {
		t.Fatal("Set with empty record: want error, got nil")
	}
	if len(job.Events()) != 0 {
		t.Fatal("Set with invalid args should not emit an event")
	}
}

func TestPrintDriverDeleteValidatesArgsBeforeEmitting(t *testing.T) {
	job := events.NewJob()
	d := NewPrint(job)

	if err := d.Delete(context.Background(), ""); err == nil {
		t.Fatal("Delete with empty record: want error, got nil")
	}
	if len(job.Events()) != 0 {
		t.Fatal("Delete with invalid args should not emit an event")
	}
}

// PrintDriver must never fail on account of DNS itself (DNS-003) — even
// back-to-back Set and Delete calls against the same job both succeed,
// leaving the job open for further steps rather than ending it.
func TestPrintDriverNeverFails(t *testing.T) {
	job := events.NewJob()
	d := NewPrint(job)

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if job.Done() {
		t.Fatal("job should not be terminal — PrintDriver only emits step events")
	}
}
