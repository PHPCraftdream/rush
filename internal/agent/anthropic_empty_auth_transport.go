package agent

import "net/http"

// dropEmptyAuthTransport removes Authorization / X-Api-Key headers whose value
// is empty. configureAnthropicAuthHeaders blanks the unused one to keep the
// process environment out of the request; the SDK then puts the blank header
// on the wire, which no endpoint should have to tolerate.
type dropEmptyAuthTransport struct{ base http.RoundTripper }

func (t dropEmptyAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var empty []string
	for _, name := range []string{"Authorization", "X-Api-Key"} {
		if vals, ok := req.Header[name]; ok && allBlank(vals) {
			empty = append(empty, name)
		}
	}
	if len(empty) > 0 {
		req = req.Clone(req.Context())
		for _, name := range empty {
			req.Header.Del(name)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func allBlank(vals []string) bool {
	for _, v := range vals {
		if v != "" {
			return false
		}
	}
	return true
}

// withoutEmptyAuthHeaders returns a copy of client (nil = the default client)
// whose transport drops blank auth headers; the shared client is not mutated.
func withoutEmptyAuthHeaders(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Transport: dropEmptyAuthTransport{}}
	}
	clone := *client
	clone.Transport = dropEmptyAuthTransport{base: client.Transport}
	return &clone
}
