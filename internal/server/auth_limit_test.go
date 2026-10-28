package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type countedBody struct {
	*bytes.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func TestAuthRejectsOversizedBodyWithoutDrainingIt(t *testing.T) {
	a := newAuth()
	bodyBytes := append([]byte(`{"token":"`+a.token+`"}`), bytes.Repeat([]byte(" "), 1<<20)...)
	body := &countedBody{Reader: bytes.NewReader(bodyBytes)}
	r := httptest.NewRequest(http.MethodPost, "/auth", body)
	w := httptest.NewRecorder()
	a.HandleAuth(w, r)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.Less(t, body.read, len(bodyBytes), "handler must reject without reading the full body")
}

func TestAuthLoginStillWorks(t *testing.T) {
	a := newAuth()
	r := httptest.NewRequest(http.MethodPost, "/auth", bytes.NewBufferString(`{"token":"`+a.token+`"}`))
	w := httptest.NewRecorder()
	a.HandleAuth(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, w.Result().Cookies())
}
