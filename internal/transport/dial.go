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

// DialArgs returns the argv for `ssh -W host:port`, which forwards the
// process's stdio to addr as seen from the deploy host.
func (r *Remote) DialArgs(addr string) ([]string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid forward address %q: %w", addr, err)
	}
	if host == "" || port == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host+port, " \t\r\n\x00") {
		return nil, fmt.Errorf("invalid forward address %q", addr)
	}
	args := append([]string{r.ssh()}, r.Target.SSHOptions()...)
	return append(args, "-W", net.JoinHostPort(host, port), "--", r.Target.Host), nil
}

// DialContext opens `ssh -W addr` and returns its stdio as a net.Conn,
// suitable as an http.Transport DialContext (network must be tcp, tcp4 or
// tcp6). It always spawns the real ssh program (Remote.SSHProgram), not
// Remote.Exec. ctx is checked before the process starts; the connection then
// lives until Close (use deadlines or Close to bound it).
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
	return startStdioConn(args, addr)
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
	stderr  *TailBuffer
	done    chan struct{}
	waitErr error
	once    sync.Once
}

func startStdioConn(args []string, addr string) (net.Conn, error) {
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
	c := &stdioConn{program: args[0], addr: addr, r: outR, w: inW, stderr: &TailBuffer{Max: stderrTail}, done: make(chan struct{})}
	c.cmd = exec.Command(args[0], args[1:]...)
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
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

// exitError waits briefly for ssh to exit and returns a *CommandError if it
// exited unsuccessfully.
func (c *stdioConn) exitError(wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		return nil
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
// promptly.
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.r.Close()
		timer := time.NewTimer(2 * time.Second)
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
