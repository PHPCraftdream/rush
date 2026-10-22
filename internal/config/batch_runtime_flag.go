// R12-H4 atomic activation: the batch runtime is the only runtime, so
// BatchRuntimeEnabled is unconditional. The RUSH_BATCH_RUNTIME symbol is
// kept for one lane so the other packages' selectors can be rewritten in
// the same series; lane R12-I erases this file afterwards.
package config

// BatchRuntimeEnabled always reports true: selection of the batch
// runtime is unconditional for every RUSH_BATCH_RUNTIME value, set or
// unset.
func BatchRuntimeEnabled() bool {
	return true
}
