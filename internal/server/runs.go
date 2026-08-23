package server

import (
	"strings"
	"sync"
	"time"
)

// Registry is every run this process is driving, addressed by the journal's run
// id.
//
// This type is the guard on decision 2: a closing browser tab does not abort a
// run, so a run cannot live in a connection. It lives here, and a tab is only
// ever a view of it.
//
// Three consequences, all of them deliberate:
//
//   - Nothing in this package cancels a run's context when a response ends.
//     There is no OnDisconnect, no r.Context() plumbed into a run, and no
//     heartbeat whose absence means anything. A tab closing is indistinguishable
//     from a reload, a laptop lid and a Wi-Fi blip, and the batch it would abort
//     is one that has peers holding reservations against outpoints.
//   - The token is per server start, not per tab, so any tab that has it can
//     attach to any run. "Joinable per run rather than per tab" is what makes a
//     reconnect work at all: the second tab is not a second session.
//   - The transcript accumulates on the Run and is rendered from the beginning
//     on every attach. So an attach is idempotent and complete rather than a
//     subscription that missed what happened while nobody was looking.
//
// What ends a run is the clock — the 5:00 gate that rehearsal.Gate measures
// against, and the peers' ten minutes, which is their clock and not ours — or
// the operator's Ctrl-C on the process. Not the transport.
//
// # It has no production caller yet
//
// The POST that starts a run is the next slice, along with the four callback
// seams. Registry and Run are built now because the shape of the attach model
// had to be decided before any handler was written, and this is the shape: the
// attach handler reads it, and nothing writes to it yet. That is the one place
// in this repository where written-but-uncalled code exists, and it is called
// out here rather than left to be discovered.
type Registry struct {
	mu   sync.Mutex
	runs map[string]*Run
}

// NewRegistry is an empty one.
func NewRegistry() *Registry {
	return &Registry{runs: map[string]*Run{}}
}

// Add puts a run in the registry, returning it so the caller can write to it.
func (reg *Registry) Add(id string) *Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	r := &Run{ID: id, Started: time.Now()}
	reg.runs[id] = r
	return r
}

// Get is the attach: a run id, and whatever that run has said so far.
func (reg *Registry) Get(id string) *Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.runs[id]
}

// List is every run, newest first, for the index.
func (reg *Registry) List() []*Run {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := make([]*Run, 0, len(reg.runs))
	for _, r := range reg.runs {
		out = append(out, r)
	}
	// Newest first: a run started ten minutes ago is history and the one started
	// a moment ago is what the operator came back for.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Started.After(out[j-1].Started); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Run is one run's screen: what it has said, and whether it is still saying it.
//
// It is an io.Writer, which is what run.Deps.Out wants. That is the whole seam
// between the composition and the UI for everything the run only reports; the
// four things it has to ask are the callback seams, and they are the next slice.
type Run struct {
	ID      string
	Started time.Time

	mu       sync.Mutex
	said     strings.Builder
	finished bool
	err      error
}

// Write appends to the transcript. It never fails: a run must not stop because
// nobody is looking at it.
func (r *Run) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.said.Write(p)
	return len(p), nil
}

// Finish records that the run is over, and how.
func (r *Run) Finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	r.err = err
}

// State is the transcript and what became of the run, read together so a screen
// cannot render a transcript from before the run ended beside a verdict from
// after it.
func (r *Run) State() (transcript string, finished bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.said.String(), r.finished, r.err
}
