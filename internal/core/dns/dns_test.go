package dns

import "testing"

func TestRecordType(t *testing.T) {
	cases := map[string]string{
		"203.0.113.10":             "A",
		"2001:db8::1":              "AAAA",
		"standby.example.com":      "CNAME",
		"another-host.example.org": "CNAME",
	}
	for value, want := range cases {
		if got := recordType(value); got != want {
			t.Errorf("recordType(%q) = %q, want %q", value, got, want)
		}
	}
}
