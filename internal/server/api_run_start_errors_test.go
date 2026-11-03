package server

import (
	"errors"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Every way the backend can refuse to start a run maps to one fixed answer;
// the refused run is recorded as failed, never left queued, and nothing the
// backend said (error text) reaches the client for the generic case.
func TestAPIRunStartErrorsMapToFixedProblems(t *testing.T) {
	apiRunHandlerEnv(t)
	cases := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{"gate unavailable fails closed", ErrAPIRunBackendGateUnavailable, http.StatusServiceUnavailable, "permission gate unavailable"},
		{"busy", ErrAPIRunBackendBusy, http.StatusConflict, "session is busy"},
		{"invalid model", ErrAPIRunBackendInvalidModel, http.StatusBadRequest, "invalid model"},
		{"invalid role", ErrAPIRunBackendInvalidRole, http.StatusBadRequest, "invalid role"},
		{"anything else is the generic 5xx", errors.New("dial tcp 10.0.0.1: secret-internal-detail"), http.StatusServiceUnavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newAPIRunHandlerBackend()
			backend.startErr = tc.err
			handler := apiRunHandler(&apiRunHandlerStore{item: session.Session{ID: "session-1"}}, backend)
			res := apiRunRequest(t, handler, http.MethodPost, "/api/v1/sessions/session-1/runs", `{"prompt":"go"}`, Principal{TokenID: "tok", Scopes: []string{"run"}}, nil)
			require.Equal(t, tc.status, res.status, string(res.body))
			if tc.detail == "" {
				require.NotContains(t, string(res.body), `"detail"`, "a generic 5xx carries no detail")
			} else {
				require.Contains(t, string(res.body), `"detail":"`+tc.detail+`"`)
			}
			require.NotContains(t, string(res.body), "secret-internal-detail")
			require.Equal(t, "application/problem+json", res.header.Get("Content-Type"))
		})
	}
}
