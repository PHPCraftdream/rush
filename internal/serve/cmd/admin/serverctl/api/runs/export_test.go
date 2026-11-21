package apiruns

import (
	"context"
	"os"
	"testing"
	"time"
)

func SetNotifyContextForTest(t *testing.T, notify func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)) {
	t.Helper()
	previous := apiNotifyContext
	apiNotifyContext = notify
	t.Cleanup(func() { apiNotifyContext = previous })
}

func SetRunStatePollIntervalForTest(t *testing.T, interval time.Duration) {
	t.Helper()
	previous := apiRunStatePollInterval
	apiRunStatePollInterval = interval
	t.Cleanup(func() { apiRunStatePollInterval = previous })
}
