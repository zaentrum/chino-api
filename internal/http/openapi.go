package http

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var openapi []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapi)
}
