// Package transporttest provides a recording fake transport.Executor.
package transporttest

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/frgmt0/depper/internal/transport"
)

// Call is one recorded command.
type Call struct {
	Args    []string
	Stdin   string
	Capture bool
	Dir     string
	Env     []string
}

// Program returns Args[0].
func (c Call) Program() string {
	if len(c.Args) == 0 {
		return ""
	}
	return c.Args[0]
}

// Recorder is a fake Executor that records every call and answers with
// Handler (nil Handler: every command succeeds with empty output). For
// streamed calls, the handler's Result.Stdout/Stderr are written to the
// Cmd's Stdout/Stderr writers when set. It is safe for concurrent use.
type Recorder struct {
	Handler func(Call) (transport.Result, error)

	mu    sync.Mutex
	calls []Call
}

// Run implements transport.Executor.
func (r *Recorder) Run(_ context.Context, c transport.Cmd) (transport.Result, error) {
	call := Call{Args: append([]string(nil), c.Args...), Capture: c.Capture, Dir: c.Dir, Env: append([]string(nil), c.Env...)}
	if c.Stdin != nil {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return transport.Result{}, err
		}
		call.Stdin = string(data)
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	handler := r.Handler
	r.mu.Unlock()
	if handler == nil {
		return transport.Result{}, nil
	}
	res, err := handler(call)
	if !c.Capture {
		if c.Stdout != nil && res.Stdout != "" {
			_, _ = io.WriteString(c.Stdout, res.Stdout)
		}
		if c.Stderr != nil && res.Stderr != "" {
			_, _ = io.WriteString(c.Stderr, res.Stderr)
		}
		res = transport.Result{}
	}
	return res, err
}

// Calls returns a copy of the recorded calls.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Scripts returns the stdin of every recorded ssh script call (argv ending
// in "sh -s"), in order.
func (r *Recorder) Scripts() []string {
	var scripts []string
	for _, c := range r.Calls() {
		if len(c.Args) > 0 && c.Args[len(c.Args)-1] == transport.RemoteShell {
			scripts = append(scripts, c.Stdin)
		}
	}
	return scripts
}

// Reset forgets all recorded calls.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

// ScriptContains is a convenience matcher for handlers.
func ScriptContains(c Call, fragment string) bool { return strings.Contains(c.Stdin, fragment) }
