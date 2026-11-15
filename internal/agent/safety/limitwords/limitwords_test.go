package limitwords

import (
	"strings"
	"testing"
	"time"
)

// Revert-check: Classify's reset branch must override StepFun's transient quota rule.
func TestClassifyResetBoundary(t *testing.T) {
	for _, tc := range []struct {
		delay time.Duration
		want  Class
	}{{29*time.Minute + 59*time.Second, Transient}, {30 * time.Minute, Hard}} {
		got := Classify(Input{Status: 429, Provider: "stepfun", Message: "quota", ResetAfter: tc.delay})
		if got != tc.want {
			t.Fatalf("delay=%s got=%v want=%v", tc.delay, got, tc.want)
		}
	}
}

// Revert-check: Classify's EqualFold code/type comparisons must accept both cases.
func TestClassifyCodexCase(t *testing.T) {
	// https://github.com/acmiyaguchi/fen/issues/583; not reproduced locally.
	for _, key := range []string{"code", "type"} {
		for _, value := range []string{"usage_limit_reached", "USAGE_LIMIT_REACHED"} {
			body := `{"error":{"` + key + `":"` + value + `","message":"wall"}}`
			for _, raw := range []string{body, "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\n\r\n" + body} {
				got := Classify(Input{Status: 0, Provider: "OPENAI-CODEX", Body: raw})
				if got != Hard {
					t.Fatalf("key=%s value=%s got=%v", key, value, got)
				}
				rate := strings.ReplaceAll(raw, value, "RATE_LIMIT_EXCEEDED")
				if got := Classify(Input{Status: 429, Message: "quota", Body: rate}); got != Transient {
					t.Fatalf("rate=%s got=%v", rate, got)
				}
			}
		}
	}
}
