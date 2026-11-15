package cmd

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

func tokenBindingPresentation(binding config.APITokenBinding) string {
	binding = binding.Normalized()
	if binding.Kind == "session" && binding.Pin != nil {
		return fmt.Sprintf("session (session=%s, workspace=%s)", binding.Pin.Session, binding.Pin.Workspace)
	}
	if binding.Kind == "batch" && binding.Batch != nil {
		return fmt.Sprintf("batch (tools=%s, max_roots=%d, max_parallel=%d)", strings.Join(binding.Batch.Tools, ","), binding.Batch.MaxRoots, binding.Batch.MaxParallel)
	}
	return binding.Kind
}
