package agent

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/session"
)

// SetCoordinatorSessionCredentialRequirementStore binds the optional durable,
// secret-free marker store used to preserve credential boundaries across restarts.
func SetCoordinatorSessionCredentialRequirementStore(c Coordinator, store session.SessionCredentialRequirementStore) error {
	if c == nil {
		return errors.New("session credential requirement store: nil coordinator")
	}
	if store == nil {
		return errors.New("session credential requirement store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("session credential requirement store: unsupported coordinator type")
	}
	co.credentialReqs = store
	return nil
}
