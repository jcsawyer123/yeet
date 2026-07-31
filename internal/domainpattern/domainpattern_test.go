package domainpattern

import "testing"

func TestResolve(t *testing.T) {
	bases := []string{"dev.jcsx.me"}

	cases := []struct {
		name    string
		pattern string
		id      string
		want    string
		wantErr bool
	}{
		{"empty pattern uses default", "", "abc123", "abc123.dev.jcsx.me", false},
		{"bare id template", "{id}", "abc123", "abc123.dev.jcsx.me", true}, // no base at all in the pattern
		{"id with base", "{id}.dev.jcsx.me", "abc123", "abc123.dev.jcsx.me", false},
		{"prefix-id", "service-{id}.dev.jcsx.me", "abc123", "service-abc123.dev.jcsx.me", false},
		{"id-suffix", "{id}-service.dev.jcsx.me", "abc123", "abc123-service.dev.jcsx.me", false},
		{"two labels deep - rejected", "sub.{id}.dev.jcsx.me", "abc123", "", true},
		{"wrong base entirely - rejected", "{id}.jcsx.me", "abc123", "", true},
		{"apex of allowed base - rejected (no label)", "dev.jcsx.me", "abc123", "", true},
		{"case insensitive base match", "{id}.DEV.JCSX.ME", "abc123", "abc123.dev.jcsx.me", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.pattern, c.id, bases)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveMultipleBases(t *testing.T) {
	bases := []string{"dev.jcsx.me", "run.jcsx.me"}

	got, err := Resolve("{id}.run.jcsx.me", "abc123", bases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123.run.jcsx.me" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveNoAllowedBases(t *testing.T) {
	if _, err := Resolve("{id}.dev.jcsx.me", "abc123", nil); err == nil {
		t.Fatal("expected error with no allowed bases configured")
	}
}
