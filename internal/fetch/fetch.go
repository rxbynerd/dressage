// Package fetch defines the provider-neutral log ingestion interface.
package fetch

import (
	"context"
	"time"

	"github.com/rxbynerd/dressage/internal/model"
)

// Fetcher retrieves normalized invocation records for the [start, end) window.
// A zero start or end means unbounded on that side.
type Fetcher interface {
	Fetch(ctx context.Context, start, end time.Time) ([]model.Record, error)
}
