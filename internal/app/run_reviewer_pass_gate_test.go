package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
)

// TestShouldRunReviewerPass is the table-level test for the reviewer-pass
// gate: a finished `rush run` is auto-followed by one Reviewer-model turn
// only when the run declared --role smart AND a Reviewer slot is
// configured with a non-empty Model. Every explicit non-smart role and
// every unconfigured-reviewer combination must keep today's behavior.
func TestShouldRunReviewerPass(t *testing.T) {
	t.Parallel()

	reviewerConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeReviewer: {Provider: "openai", Model: "o4-mini"},
		},
	}
	reviewerEmptyModel := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			// Present but Model == "" must NOT count as configured.
			config.SelectedModelTypeReviewer: {Provider: "openai", Model: ""},
		},
	}
	reviewerNotConfigured := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{},
	}

	tests := []struct {
		name string
		role config.SelectedModelType
		cfg  *config.Config
		want bool
	}{
		// --- The feature's trigger case ---
		{
			name: "reviewer configured, role smart => review pass",
			role: config.SelectedModelTypeSmart,
			cfg:  reviewerConfigured,
			want: true,
		},

		// --- reviewer configured x every explicit non-smart role ---
		{
			name: "reviewer configured, role fast => no review pass",
			role: config.SelectedModelTypeFast,
			cfg:  reviewerConfigured,
			want: false,
		},
		{
			name: "reviewer configured, role worker => no review pass",
			role: config.SelectedModelTypeWorker,
			cfg:  reviewerConfigured,
			want: false,
		},
		{
			name: "reviewer configured, role reviewer => no review pass (operator already chose the reviewer slot)",
			role: config.SelectedModelTypeReviewer,
			cfg:  reviewerConfigured,
			want: false,
		},

		// --- Backward compat: no reviewer configured ---
		{
			name: "reviewer NOT configured, role smart => no review pass (byte-identical legacy behavior)",
			role: config.SelectedModelTypeSmart,
			cfg:  reviewerNotConfigured,
			want: false,
		},
		{
			name: "reviewer present but Model empty, role smart => no review pass",
			role: config.SelectedModelTypeSmart,
			cfg:  reviewerEmptyModel,
			want: false,
		},

		// --- nil config guard ---
		{
			name: "nil config never triggers the review pass",
			role: config.SelectedModelTypeSmart,
			cfg:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldRunReviewerPass(tt.role, tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}
