// Package transport runs local programs (ssh, rsync, git, ...) through an
// injectable Executor, and builds the ssh/rsync invocations that reach the
// deploy host. Remote scripts are always sent on stdin, never in argv.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/frgmt0/depper/internal/shell"
)

// Cmd describes one local process invocation.
type Cmd struct {
	// Args is the argv; Args[0] is the program, looked up on PATH.
	Args []string
	// Stdin is fed to the process; nil means no input.
	Stdin io.Reader
	// Capture collects stdout and stderr into the Result instead of streaming.
	Capture bool
	// Stdout and Stderr receive the streams when Capture is false; nil means
	// os.Stdout / os.Stderr.
	Stdout io.Writer
	Stderr io.Writer
	// Dir is the working directory; "" means the current directory.
	Dir string
	// Env is the process environment; nil inherits the current environment.
	Env []string
}

// Result is what a finished process produced. Stdout and Stderr are only
// populated for captured commands.
type Result struct {
	Stdout string
	Stderr string
}

// Executor runs commands. Implementations: Local (real processes), DryRun
// (prints commands), and transporttest.Recorder (tests).
type Executor interface {
	Run(ctx context.Context, cmd Cmd) (Result, error)
}

// CommandError reports a process that exited unsuccessfully.
type CommandError struct {
	Program  string
	ExitCode int
	// Stderr holds the captured stderr, or for streamed commands the last
	// 64 KiB of it (already shown to the user).
	Stderr string
	// Stdout holds captured stdout (captured commands only).
	Stdout string
	// Streamed reports that stderr was already written to the user.
	Streamed bool
}

func (e *CommandError) Error() string {
	if !e.Streamed {
		if msg := strings.TrimSpace(e.Stderr); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(e.Stdout); msg != "" {
			return msg
		}
	}
	return fmt.Sprintf("%s exited with status %d", e.Program, e.ExitCode)
}

// NotFoundError reports a program missing from PATH.
type NotFoundError struct{ Program string }

func (e *NotFoundError) Error() string { return "required program not found: " + e.Program }

// Local runs real processes with os/exec.
type Local struct{}

const stderrTail = 64 << 10

// Run implements Executor.
func (Local) Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Args) == 0 {
		return Result{}, errors.New("transport: empty command")
	}
	program := c.Args[0]
	cmd := exec.CommandContext(ctx, program, c.Args[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	var stdout, stderr bytes.Buffer
	tail := &TailBuffer{Max: stderrTail}
	if c.Capture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdout = orDefault(c.Stdout, os.Stdout)
		cmd.Stderr = io.MultiWriter(orDefault(c.Stderr, os.Stderr), tail)
	}
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return result, &NotFoundError{Program: program}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, fmt.Errorf("%s: %w", program, ctxErr)
		}
		cerr := &CommandError{Program: program, ExitCode: exitErr.ExitCode(), Stderr: result.Stderr, Stdout: result.Stdout}
		if !c.Capture {
			cerr.Stderr, cerr.Streamed = tail.String(), true
		}
		return result, cerr
	}
	return result, fmt.Errorf("running %s: %w", program, err)
}

func orDefault(w io.Writer, def io.Writer) io.Writer {
	if w == nil {
		return def
	}
	return w
}

// DryRun prints each command instead of running it, like the Python
// Runner's dry-run mode, and returns an empty successful Result. Stdin
// (remote scripts, which may embed secrets) is never printed.
type DryRun struct {
	Out io.Writer // nil means os.Stdout
}

// Run implements Executor.
func (d DryRun) Run(_ context.Context, c Cmd) (Result, error) {
	_, err := fmt.Fprintln(orDefault(d.Out, os.Stdout), "DRY RUN:", shell.Join(c.Args...))
	return Result{}, err
}

// TailBuffer is an io.Writer that keeps the last Max bytes written. It is
// safe for concurrent use.
type TailBuffer struct {
	Max int
	mu  sync.Mutex
	buf []byte
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if t.Max > 0 && len(t.buf) > t.Max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.Max:]...)
	}
	return len(p), nil
}

// String returns the retained bytes.
func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
