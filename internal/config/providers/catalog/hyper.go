package catalog

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"log/slog"
	"os"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
)

// HyperName is the default name of this meta provider.
const HyperName = "hyper"

// hyperDefaultBaseURL is the default proxy URL.
const hyperDefaultBaseURL = "https://hyper.charm.land"

// hyperBaseURLFrom resolves the base URL from a HYPER_URL value.
func hyperBaseURLFrom(envValue string) string {
	return cmp.Or(envValue, hyperDefaultBaseURL)
}

// HyperBaseURL returns the base URL, which is either $HYPER_URL or the default.
var HyperBaseURL = sync.OnceValue(func() string {
	return hyperBaseURLFrom(os.Getenv("HYPER_URL"))
})

//go:generate wget -O provider.json https://hyper.charm.land/api/v1/provider

//go:embed provider.json
var embedded []byte

// hyperEmbeddedFromBytes decodes embedded provider data, applying the
// HYPER_URL endpoint override when set.
func hyperEmbeddedFromBytes(data []byte, hyperURL string) catwalk.Provider {
	var provider catwalk.Provider
	if err := json.Unmarshal(data, &provider); err != nil {
		slog.Error("Could not use embedded provider data", "err", err)
	}
	if hyperURL != "" {
		provider.APIEndpoint = hyperURL + "/api/v1/fantasy"
	}
	return provider
}

// Embedded returns the embedded Hyper provider.
var HyperEmbedded = sync.OnceValue(func() catwalk.Provider {
	return hyperEmbeddedFromBytes(embedded, os.Getenv("HYPER_URL"))
})
