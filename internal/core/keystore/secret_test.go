package keystore

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const testValue = "super-secret-value"

func TestSecretRevealRoundTrips(t *testing.T) {
	s := NewSecret(testValue)
	if got := s.Reveal(); got != testValue {
		t.Fatalf("Reveal() = %q, want %q", got, testValue)
	}
}

func TestSecretStringRedacts(t *testing.T) {
	s := NewSecret(testValue)
	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprint(s),
	} {
		if strings.Contains(got, testValue) {
			t.Fatalf("formatted output %q leaks the secret value", got)
		}
		if got != redacted {
			t.Fatalf("formatted output = %q, want %q", got, redacted)
		}
	}
}

func TestSecretGoStringRedacts(t *testing.T) {
	s := NewSecret(testValue)
	got := fmt.Sprintf("%#v", s)
	if strings.Contains(got, testValue) {
		t.Fatalf("%%#v output %q leaks the secret value", got)
	}
}

func TestSecretNestedInStructRedacts(t *testing.T) {
	type holder struct {
		Key Secret
	}
	h := holder{Key: NewSecret(testValue)}
	for _, got := range []string{
		fmt.Sprintf("%v", h),
		fmt.Sprintf("%+v", h),
		fmt.Sprintf("%#v", h),
	} {
		if strings.Contains(got, testValue) {
			t.Fatalf("struct formatting %q leaks the secret value", got)
		}
	}
}

func TestSecretInWrappedErrorRedacts(t *testing.T) {
	s := NewSecret(testValue)
	err := fmt.Errorf("resolve failed for %v", s)
	if strings.Contains(err.Error(), testValue) {
		t.Fatalf("wrapped error %q leaks the secret value", err.Error())
	}
}

func TestSecretMarshalJSONRedacts(t *testing.T) {
	s := NewSecret(testValue)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), testValue) {
		t.Fatalf("JSON %q leaks the secret value", b)
	}

	type holder struct {
		Key Secret `json:"key"`
	}
	b, err = json.Marshal(holder{Key: s})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), testValue) {
		t.Fatalf("nested JSON %q leaks the secret value", b)
	}
}

func TestSecretMarshalYAMLRedacts(t *testing.T) {
	type holder struct {
		Key Secret `yaml:"key"`
	}
	b, err := yaml.Marshal(holder{Key: NewSecret(testValue)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), testValue) {
		t.Fatalf("YAML %q leaks the secret value", b)
	}
}

func TestSecretZeroValueRedacts(t *testing.T) {
	var s Secret
	if s.String() != redacted {
		t.Fatalf("zero-value String() = %q, want %q", s.String(), redacted)
	}
	if s.Reveal() != "" {
		t.Fatalf("zero-value Reveal() = %q, want empty", s.Reveal())
	}
}
