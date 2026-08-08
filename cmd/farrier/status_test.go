package main

import (
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/status"
)

func TestPrintReportReportsUnmeasuredLag(t *testing.T) {
	var buf strings.Builder
	printReport(&buf, status.Report{
		TLS:  status.TLSStatus{Valid: true, NotAfter: time.Now().Add(90 * 24 * time.Hour)},
		Disk: status.DiskStatus{Path: "/"},
		Lag:  status.Lag{State: status.LagUnmeasured},
	})

	out := buf.String()
	if !strings.Contains(out, "replication lag:") {
		t.Errorf("output missing replication lag section: %q", out)
	}
	if !strings.Contains(out, "unmeasured") {
		t.Errorf("output = %q, want it to report unmeasured lag", out)
	}
}

func TestPrintReportReportsMeasuredLag(t *testing.T) {
	var buf strings.Builder
	lastBackup := time.Now().Add(-3 * time.Hour)
	printReport(&buf, status.Report{
		TLS:  status.TLSStatus{Valid: true, NotAfter: time.Now().Add(90 * 24 * time.Hour)},
		Disk: status.DiskStatus{Path: "/"},
		Lag:  status.Lag{State: status.LagMeasured, LastBackup: lastBackup, Age: 3 * time.Hour},
	})

	out := buf.String()
	if !strings.Contains(out, "last backup:") {
		t.Errorf("output = %q, want it to report the last backup age", out)
	}
	if strings.Contains(out, "clock skew") {
		t.Errorf("output = %q, want no clock skew line when Skew is zero", out)
	}
}

func TestPrintReportReportsNoBackupsLag(t *testing.T) {
	var buf strings.Builder
	printReport(&buf, status.Report{
		TLS:  status.TLSStatus{Valid: true, NotAfter: time.Now().Add(90 * 24 * time.Hour)},
		Disk: status.DiskStatus{Path: "/"},
		Lag:  status.Lag{State: status.LagNoBackups},
	})

	out := buf.String()
	if !strings.Contains(out, "no backups yet") {
		t.Errorf("output = %q, want it to report no backups yet", out)
	}
}
