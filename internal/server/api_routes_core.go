package server

import (
	"net/http"

	"github.com/PHPCraftdream/rush/internal/version"
)

func apiHealthHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func apiVersionHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, struct {
		Version string `json:"version"`
	}{Version: version.VersionLine()})
}

func apiOpenAPIHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, BuildOpenAPIDocument(APIRoutes()))
}
