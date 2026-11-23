// Package catalog holds static provider reference data: the local CLI
// provider identity, and the embedded Hyper provider catalog. It imports
// only the standard library so both internal/config and internal/agent
// provider packages can depend on it.
package catalog

// LocalCLIProviderType is the catwalk.Type value used for CLI providers.
const LocalCLIProviderType = "cli"

// LocalCLIProviderID is the catwalk.InferenceProvider ID for the built-in
// CLI provider.
const LocalCLIProviderID = "local-cli"

// LocalCLIModel is the static, execution-free projection of one CLI model
// spec: the fields config synthesizes the local-cli provider's model list
// from. Runtime availability stays with the owning agent package.
type LocalCLIModel struct {
	ID            string
	Name          string
	ContextWindow int64
}
