package orchestrate

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Target
		wantErr bool
	}{
		{name: "user and host", raw: "ssh://deploy@example.com", want: Target{User: "deploy", Host: "example.com", Port: "22"}},
		{name: "explicit port", raw: "ssh://deploy@example.com:2222", want: Target{User: "deploy", Host: "example.com", Port: "2222"}},
		{name: "ip host", raw: "ssh://deploy@10.0.0.5", want: Target{User: "deploy", Host: "10.0.0.5", Port: "22"}},
		{name: "missing scheme", raw: "deploy@example.com", wantErr: true},
		{name: "wrong scheme", raw: "https://deploy@example.com", wantErr: true},
		{name: "missing user", raw: "ssh://example.com", wantErr: true},
		{name: "missing host", raw: "ssh://deploy@", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTarget(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q) = %+v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseTarget(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}
