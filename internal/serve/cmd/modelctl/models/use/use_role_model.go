package modeluse

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
)

// ResolveModelRole maps a --role value (as accepted by `rush run` and
// `rush ping`) to the model slot it selects. The empty string is rejected
// here; callers that want to default it (e.g. `rush ping`) must handle ""
// before calling. Keeping the vocabulary and error text in one place stops
// the two commands' role parsing from drifting apart.
//
// "reviewer" is the strongest model slot and is never selected
// automatically — it must be requested explicitly via --role reviewer by
// an operator or a senior agent. "worker" is a cheap slot intended for
// manual sub-task delegation.
func ResolveModelRole(role string) (config.SelectedModelType, error) {
	switch role {
	case string(config.SelectedModelTypeSmart):
		return config.SelectedModelTypeSmart, nil
	case string(config.SelectedModelTypeFast):
		return config.SelectedModelTypeFast, nil
	case string(config.SelectedModelTypeWorker):
		return config.SelectedModelTypeWorker, nil
	case string(config.SelectedModelTypeReviewer):
		return config.SelectedModelTypeReviewer, nil
	default:
		return "", fmt.Errorf("--role: invalid value %q (allowed: smart, fast, worker, reviewer)", role)
	}
}
