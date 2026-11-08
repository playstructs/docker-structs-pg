// Package timing collects optional, operation-local timings. A recorder belongs
// to one sequential database writer; it is never shared with fetch goroutines.
package timing

import (
	"context"
	"time"
)

type contextKey struct{}

type Recorder struct {
	Durations map[string]time.Duration
	Counts    map[string]int64
}

func New(ctx context.Context) (context.Context, *Recorder) {
	r := &Recorder{Durations: make(map[string]time.Duration), Counts: make(map[string]int64)}
	return context.WithValue(ctx, contextKey{}, r), r
}

// Phases returns a sequential phase switch. Call next("") on exit, including
// errors, so the unfinished phase is measured. Independent nested scopes may
// contribute subphase names; those times must not be added to their parent.
func Phases(ctx context.Context) func(string) {
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	if r == nil {
		return func(string) {}
	}
	var name string
	var started time.Time
	return func(next string) {
		if name != "" {
			r.Durations[name] += time.Since(started)
		}
		name, started = next, time.Now()
	}
}

func Count(ctx context.Context, name string, n int64) {
	if r, _ := ctx.Value(contextKey{}).(*Recorder); r != nil {
		r.Counts[name] += n
	}
}
