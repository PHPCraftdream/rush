package shell

import "sync"

var (
	denyExecMu     sync.RWMutex
	denyExecPolicy func([]string) error
)

// RegisterCommandPolicy installs the immutable parsed-once process policy.
func RegisterCommandPolicy(check func([]string) error) {
	denyExecMu.Lock()
	defer denyExecMu.Unlock()
	if denyExecPolicy == nil {
		denyExecPolicy = check
	}
}

func checkRegisteredCommandPolicy(args []string) error {
	denyExecMu.RLock()
	check := denyExecPolicy
	denyExecMu.RUnlock()
	if check == nil {
		return nil
	}
	return check(args)
}
