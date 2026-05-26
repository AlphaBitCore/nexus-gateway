// shutdown.go — graceful shutdown helpers.
package wiring

import (
	"context"
	"net/http"
	"time"
)

// RunServer starts the HTTP server and blocks until ctx is cancelled or an
// error occurs. It returns the server error (nil on clean shutdown).
func RunServer(ctx context.Context, server *http.Server) error {
	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}
