package provpeak

import "testing"

func TestEscapeSJSONPathComponent_SingleBackslash(t *testing.T) {
	for _, input := range []string{`provider\id`, `api\key`} {
		t.Run(input, func(t *testing.T) {
			got := EscapeSJSONPathComponent(input)
			want := `provider\\id`
			if input == `api\key` {
				want = `api\\key`
			}
			if got != want {
				t.Errorf("EscapeSJSONPathComponent(%q) = %q, want %q", input, got, want)
			}
		})
	}
}
