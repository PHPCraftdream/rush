package cliprovider

import "context"

// Both callbacks are test-only, independent seams: registration tests observe
// the post-permission boundary; runShell tests observe its own policy guard.
var (
	testMCPBashSpawn  func(context.Context, string, string) (string, error)
	testRunShellSpawn func(context.Context, string, string) (string, error)
)
