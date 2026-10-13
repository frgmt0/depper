package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ForwardReadyMarker is the stderr line `ssh -W` prints (through
// LocalCommand) once it has connected and authenticated, so DialContext can
// tell the SSH handshake apart from the forwarded request.
const ForwardReadyMarker = "depper-forward-ready"

// DialArgs returns the argv for `ssh -W host:port`, which forwards the
// process's stdio to addr as seen from the deploy host.
//
// ssh runs LocalCommand after connecting and authenticating; it prints
// ForwardReadyMarker on stderr (stdout is the forwarded stream). Connection
// multiplexing is disabled because a multiplexed client never runs
// LocalCommand.
func (r *Remote) DialArgs(addr string) ([]string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid forward address %q: %w", addr, err)
	}
	if host == "" || port == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host+port, " \t\r\n\x00") {
		return nil, fmt.Errorf("invalid forward address %q", addr)
	}
	args := append([]string{r.ssh()}, r.Target.SSHOptions()...)
	args = append(args,
		"-o", "ControlPath=none",
		"-o", "PermitLocalCommand=yes",
		"-o", "LocalCommand=echo "+ForwardReadyMarker+" 1>&2",
	)
	return append(args, "-W", net.JoinHostPort(host, port), "--", r.Target.Host), nil
}

// DialContext opens `ssh -W addr` and returns its stdio as a net.Conn,
// suitable as an http.Transport DialContext (network must be tcp, tcp4 or
// tcp6). It always spawns the real ssh program (Remote.SSHProgram), not
// Remote.Exec. DialContext returns once ssh reports that it is connected
// (ForwardReadyMarker), when ssh exits (its error), or when ctx is done; so
// a deadline on ctx bounds the SSH handshake only. The connection then lives
// until Close (use deadlines or Close to bound it).
//
// When the forward fails, Read/Write return a *CommandError carrying ssh's
// stderr; IsForwardingProhibited distinguishes a server that disallows
// forwarding from a closed target port.
func (r *Remote) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("ssh forwarding supports tcp only, not %q", network)
	}
	args, err := r.DialArgs(addr)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := startStdioConn(args, addr)
	if err != nil {
		return nil, err
	}
	select {
	case <-c.ready:
		return c, nil
	case <-c.done:
		// ssh exited before connecting (or before LocalCommand ran).
		select {
		case <-c.ready:
			return c, nil
		default:
		}
		err := c.exitError(0)
		_ = c.Close()
		if err == nil {
			err = fmt.Errorf("%s exited before the forward to %s was established", c.program, addr)
		}
		return nil, err
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

// IsForwardingProhibited reports whether err shows the SSH server refused
// TCP forwarding (AllowTcpForwarding no: "administratively prohibited"), as
// opposed to the target refusing the connection. Callers fall back to remote
// curl in that case.
func IsForwardingProhibited(err error) bool {
	var cerr *CommandError
	if !errors.As(err, &cerr) {
		return false
	}
	return strings.Contains(cerr.Stderr, "administratively prohibited")
}

type stdioAddr string

func (a stdioAddr) Network() string { return "ssh-stdio" }
func (a stdioAddr) String() string  { return string(a) }

type stdioConn struct {
	cmd     *exec.Cmd
	program string
	addr    string
	r       *os.File // read end of the child's stdout
	w       *os.File // write end of the child's stdin
	stderr  *readyWriter
	ready   chan struct{}
	done    chan struct{}
	waitErr error
	once    sync.Once
}

// readyWriter keeps ssh's stderr (in a TailBuffer) except the
// ForwardReadyMarker line, whose arrival closes ready.
type readyWriter struct {
	tail  *TailBuffer
	ready chan struct{}
	mu    sync.Mutex
	line  []byte
	seen  bool
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		w.line = append(w.line, b)
		if b != '\n' {
			continue
		}
		w.flushLine()
	}
	return len(p), nil
}

// flushLine moves the buffered line to the tail, unless it is the marker.
func (w *readyWriter) flushLine() {
	line := w.line
	w.line = nil
	if !w.seen && strings.TrimRight(string(line), "\r\n") == ForwardReadyMarker {
		w.seen = true
		close(w.ready)
		return
	}
	_, _ = w.tail.Write(line)
}

// String returns the retained stderr, including an unterminated last line.
func (w *readyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tail.String() + string(w.line)
}

func startStdioConn(args []string, addr string) (*stdioConn, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	ready := make(chan struct{})
	c := &stdioConn{program: args[0], addr: addr, r: outR, w: inW, ready: ready, done: make(chan struct{}),
		stderr: &readyWriter{tail: &TailBuffer{Max: stderrTail}, ready: ready}}
	c.cmd = exec.Command(args[0], args[1:]...)
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
	// A leftover child holding ssh's stderr open must not keep Wait (and
	// so Close) blocked.
	c.cmd.WaitDelay = WaitDelay
	ownProcessGroup(c.cmd)
	err = c.cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		outR.Close()
		inW.Close()
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &NotFoundError{Program: args[0]}
		}
		return nil, fmt.Errorf("starting %s: %w", args[0], err)
	}
	go func() {
		c.waitErr = c.cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// exitError waits up to wait for ssh to exit and returns a *CommandError if
// it exited unsuccessfully.
func (c *stdioConn) exitError(wait time.Duration) error {
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			return nil
		}
	} else {
		<-c.done
	}
	var exitErr *exec.ExitError
	if errors.As(c.waitErr, &exitErr) {
		return &CommandError{Program: c.program, ExitCode: exitErr.ExitCode(), Stderr: c.stderr.String()}
	}
	return nil
}

func (c *stdioConn) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if errors.Is(err, io.EOF) {
		if cerr := c.exitError(2 * time.Second); cerr != nil {
			return n, cerr
		}
	}
	return n, err
}

func (c *stdioConn) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, os.ErrClosed) {
		if cerr := c.exitError(2 * time.Second); cerr != nil {
			return n, cerr
		}
	}
	return n, err
}

// Close closes both pipes and reaps ssh, killing it if it does not exit
// promptly. It returns within about 2*WaitDelay: Wait itself gives up on
// leftover pipe holders after WaitDelay.
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.r.Close()
		timer := time.NewTimer(WaitDelay)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
	return nil
}

func (c *stdioConn) LocalAddr() net.Addr  { return stdioAddr("ssh") }
func (c *stdioConn) RemoteAddr() net.Addr { return stdioAddr(c.addr) }

func (c *stdioConn) SetDeadline(t time.Time) error {
	if err := c.r.SetReadDeadline(t); err != nil {
		return err
	}
	return c.w.SetWriteDeadline(t)
}

func (c *stdioConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }
