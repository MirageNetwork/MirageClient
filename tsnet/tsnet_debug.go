//go:build debug

package tsnet

import "net/http"

func init() {
	altDebugServer = debugServer
}

func debugServer(handler http.Handler) {
	go func() {
		s := &http.Server{
			Addr:    ":6060",
			Handler: handler,
		}
		s.ListenAndServe()
	}()
}
