package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"sync"
	"time"
)

// pipeDrain bounds how long RunPhase waits for output after the process
// group is gone. A descendant that escaped the group (setsid) could keep the
// pipe open forever; the phase result must not depend on it.
const pipeDrain = 2 * time.Second

// BoundedOutput keeps the first limit bytes written to it, hashes exactly
// those bytes, and counts and discards the rest. It is safe for concurrent use.
type BoundedOutput struct {
	mu    sync.Mutex
	limit int64
	buf   []byte
	total int64
	h     hash.Hash
}

// NewBoundedOutput returns an output that retains at most limit bytes.
func NewBoundedOutput(limit int64) *BoundedOutput {
	return &BoundedOutput{limit: max(limit, 0), h: sha256.New()}
}

// Write accepts every byte, so a chatty child never blocks on the limit.
func (o *BoundedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if room := o.limit - int64(len(o.buf)); room > 0 {
		keep := p[:min(int64(len(p)), room)]
		o.buf = append(o.buf, keep...)
		o.h.Write(keep)
	}
	o.total += int64(len(p))
	return len(p), nil
}

// Digest is the hex SHA-256 of the retained bytes.
func (o *BoundedOutput) Digest() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return hex.EncodeToString(o.h.Sum(nil))
}

// Total is the number of bytes the processes produced.
func (o *BoundedOutput) Total() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total
}

// Truncated reports whether output beyond the limit was discarded.
func (o *BoundedOutput) Truncated() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total > int64(len(o.buf))
}

// Bytes returns a copy of the retained output.
func (o *BoundedOutput) Bytes() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.buf...)
}

// PhaseSpec is one shell command to run. Env is the complete child
// environment; nothing is inherited from the daemon.
type PhaseSpec struct {
	Command string
	Dir     string
	Env     []string
	Timeout time.Duration
}

// PhaseResult is how a phase ended. ExitStatus is nil when the process did
// not exit normally (timeout, cancellation, signal or failure to start).
type PhaseResult struct {
	ExitStatus *int64
	Duration   time.Duration
	TimedOut   bool
	Cancelled  bool
	// StartErr is set when the shell could not be started at all.
	StartErr error
}

// OK reports a clean exit 0.
func (r PhaseResult) OK() bool {
	return r.ExitStatus != nil && *r.ExitStatus == 0 && !r.TimedOut && !r.Cancelled && r.StartErr == nil
}
