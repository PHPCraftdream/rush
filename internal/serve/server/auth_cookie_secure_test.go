package server

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func sessionCookieFor(t *testing.T, a *Auth, withTLS bool) *http.Cookie {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth", bytes.NewBufferString(`{"token":"`+a.token+`"}`))
	if withTLS {
		r.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	a.HandleAuth(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("session cookie not set")
	return nil
}

func TestAuthCookieSecureOverTLS(t *testing.T) {
	t.Parallel()
	c := sessionCookieFor(t, newAuth(), true)
	require.True(t, c.Secure, "cookie issued on a TLS connection must be Secure")
	require.True(t, c.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, c.SameSite)
}

func TestAuthCookieNotSecureOverCleartext(t *testing.T) {
	t.Parallel()
	c := sessionCookieFor(t, newAuth(), false)
	require.False(t, c.Secure, "Secure cookie would be dropped by browsers on cleartext loopback")
	require.True(t, c.HttpOnly)
}
