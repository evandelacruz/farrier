package blob

import (
	"testing"
)

func TestNewLocal(t *testing.T) {
	a, err := New("local", map[string]any{"path": t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.(*LocalAdapter); !ok {
		t.Errorf("New(\"local\", ...) = %T, want *LocalAdapter", a)
	}
}

func TestNewLocalMissingPath(t *testing.T) {
	if _, err := New("local", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.path, got nil")
	}
}

func TestNewS3(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	a, err := New("s3", map[string]any{
		"bucket":    "snapshots",
		"endpoint":  "s3.example.com",
		"region":    "us-east-1",
		"pathStyle": true,
		"ssl":       false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.(*S3Adapter); !ok {
		t.Errorf("New(\"s3\", ...) = %T, want *S3Adapter", a)
	}
}

func TestNewS3MissingCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	_, err := New("s3", map[string]any{
		"bucket":   "snapshots",
		"endpoint": "s3.example.com",
	})
	if err == nil {
		t.Fatal("New: want error for missing AWS credentials, got nil")
	}
}

func TestNewS3MissingBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	_, err := New("s3", map[string]any{"endpoint": "s3.example.com"})
	if err == nil {
		t.Fatal("New: want error for missing config.bucket, got nil")
	}
}

func TestNewExecDriver(t *testing.T) {
	a, err := New("acme-blob", map[string]any{
		"path": "/usr/local/bin/acme-blob-driver",
		"args": []any{"--flag"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.(*ExecAdapter); !ok {
		t.Errorf("New(\"acme-blob\", ...) = %T, want *ExecAdapter", a)
	}
}

func TestNewExecDriverMissingPath(t *testing.T) {
	if _, err := New("acme-blob", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.path, got nil")
	}
}

func TestNewEmptyDriverName(t *testing.T) {
	if _, err := New("", map[string]any{}); err == nil {
		t.Fatal("New: want error for empty driver name, got nil")
	}
}
