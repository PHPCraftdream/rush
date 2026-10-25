package app

import "github.com/PHPCraftdream/rush/internal/agent"

// Ephemeral (dataDir == "") library-mode Apps run with the batch runtime
// always installed (since H4), but they have no batchInstall.DiskBindings,
// so a custom-DiskProvider run has nothing to bind against. These two
// helpers give such an App its own App-owned registry, without any
// filesystem-backed host infra.
//
// resolveDiskBindings returns the DiskProviderRegistry a custom-DiskProvider
// run binds its per-call Call owner against (R12-H2
// §1.2, see app_run_setup.go's BindCallDiskProvider site). A persisted App
// (dataDir != "") uses the batch runtime install's registry, exactly as
// before this seam existed; an ephemeral library-mode App (dataDir == "")
// falls back to the in-memory registry built in New. A nil return means no
// registry is available and the caller must keep failing closed with
// agent.ErrDiskProviderUnavailable.
func (app *App) resolveDiskBindings() *agent.DiskProviderRegistry {
	if app.batchInstall != nil && app.batchInstall.DiskBindings != nil {
		return app.batchInstall.DiskBindings
	}
	return app.ephemeralDiskBindings
}

// newEphemeralDiskBindings builds the ephemeral App's binding registry.
// DiskProviderRegistry (internal/agent/disk_provider_capability.go) is a
// purely process-local in-memory capability manager: it keeps no hosts/
// directory, takes no lock files and touches no database, so constructing
// one for an ephemeral (dataDir == "") App adds no filesystem-backed host
// infra. It has no Close method (GC-managed); it is closed implicitly with
// the App itself.
func newEphemeralDiskBindings() *agent.DiskProviderRegistry {
	return agent.NewDiskProviderRegistry()
}
