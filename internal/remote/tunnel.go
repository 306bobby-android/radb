// Package remote sets up the link between the machine holding the device and
// the machine running the tools.
package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// Default ports. 5037 is adb's own server port; 5554 is the port stock fastboot
// assumes when you write `-s tcp:host` without one.
const (
	DefaultADBPort      = 5037
	DefaultFastbootPort = 5554
	// DefaultProxyPort is where radb's adb proxy listens on the device host.
	// The real adb server keeps 5037 to itself, so anything local is
	// unaffected; only the tunnel is pointed at the proxy.
	DefaultProxyPort = 5038
)

// A Forward maps a port on the remote server's loopback to one on this machine.
type Forward struct {
	Remote int
	Local  int
}

// Tunnel keeps an ssh reverse forward alive, so that connecting to a port on
// the remote server's loopback reaches the matching port on this machine.
type Tunnel struct {
	// Target is the ssh destination, e.g. "bobby@build-box".
	Target string
	// Forwards are the reverse forwards to keep open.
	Forwards []Forward
	// Args are extra options handed to ssh, e.g. []string{"-p", "2222"}.
	Args []string
	Log  *slog.Logger
}

// command builds the ssh invocation.
func (t *Tunnel) command(ctx context.Context) *exec.Cmd {
	args := []string{
		"-N", // no remote command, just the forwards
		// Fail fast if the remote port is still held by a dying session,
		// rather than sitting there looking connected but forwarding nothing.
		"-o", "ExitOnForwardFailure=yes",
		// Notice a dead link within about a minute instead of hanging on a
		// half-open TCP connection.
		"-o", "ServerAliveInterval=20",
		"-o", "ServerAliveCountMax=3",
	}
	for _, f := range t.Forwards {
		args = append(args, "-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f.Remote, f.Local))
	}
	args = append(args, t.Args...)
	args = append(args, t.Target)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	// Ask ssh to close the connection rather than killing it outright, so the
	// server drops the forwards straight away instead of waiting to notice a
	// dead peer. WaitDelay is the backstop if it ignores that.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

// Run holds the tunnel open, reconnecting with backoff, until ctx is cancelled.
func (t *Tunnel) Run(ctx context.Context) error {
	const (
		minWait = time.Second
		maxWait = 30 * time.Second
		// A session that lasted this long counts as healthy, so the next
		// failure starts backing off from scratch.
		stable = time.Minute
	)
	wait := minWait

	for {
		if ctx.Err() != nil {
			return nil
		}
		t.Log.Info("connecting", "target", t.Target, "forwards", t.Forwards)
		start := time.Now()
		err := t.command(ctx).Run()
		if ctx.Err() != nil {
			return nil
		}
		up := time.Since(start)
		if up >= stable {
			wait = minWait
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				t.Log.Warn("ssh exited", "code", ee.ExitCode(), "up", up.Round(time.Second))
			} else {
				return fmt.Errorf("run ssh: %w", err)
			}
		} else {
			t.Log.Warn("ssh exited cleanly", "up", up.Round(time.Second))
		}

		t.Log.Info("retrying", "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if wait *= 2; wait > maxWait {
			wait = maxWait
		}
	}
}
