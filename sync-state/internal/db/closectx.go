package db

import (
	"context"
	"time"
)

// closeCtx returns a short-lived context for graceful close operations.
func closeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
