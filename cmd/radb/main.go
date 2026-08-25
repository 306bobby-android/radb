// Command radb lends a locally attached Android device to a remote machine, so
// that stock adb and fastboot running there drive it as if it were plugged in.
//
// adb needs no protocol work: its client already talks to a remote server over
// TCP, so tunnelling port 5037 and pointing ADB_SERVER_SOCKET at it is enough.
// fastboot has no client/server split, but it does know how to reach a
// bootloader over TCP, so radb speaks that protocol and relays it to USB.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/306bobby-android/radb/internal/adbproxy"
	"github.com/306bobby-android/radb/internal/fastboot"
	"github.com/306bobby-android/radb/internal/remote"
)

const usage = `radb lends a USB-attached Android device to a remote machine.

Run on the machine with the device plugged in:
  radb link USER@HOST        bring it all up: bridge, adb proxy and ssh tunnel
  radb serve                 only the local half, for tunnelling some other way
  radb devices               list bootloaders visible on USB
  radb doctor                check both halves of the setup

Run anywhere:
  radb remote-env            print the shell setup for the remote machine

Use "radb COMMAND -h" for the flags of a command.
`

func main() {
	log := newLogger(false)

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "link":
		err = link(ctx, os.Args[2:], log)
	case "serve":
		err = serve(ctx, os.Args[2:], log)
	case "devices":
		err = devices()
	case "doctor":
		err = doctor(os.Args[2:])
	case "remote-env":
		err = remoteEnv(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "radb: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "radb: %v\n", err)
		os.Exit(1)
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// localOpts configures the half of radb that runs on the machine holding the
// device: the fastboot bridge, the adb proxy, and the adb server behind it.
type localOpts struct {
	fastbootAddr string
	proxyAddr    string
	upstream     string
	serial       string
	timeout      time.Duration
	startADB     bool
	inject       bool
}

// bindLocalFlags registers the options that serve and link share.
func bindLocalFlags(fs *flag.FlagSet, o *localOpts) {
	fs.StringVar(&o.fastbootAddr, "addr", fmt.Sprintf("127.0.0.1:%d", remote.DefaultFastbootPort),
		"address for the fastboot bridge; loopback is right when a tunnel carries it")
	fs.StringVar(&o.proxyAddr, "adb-proxy", fmt.Sprintf("127.0.0.1:%d", remote.DefaultProxyPort),
		"address for the adb proxy the tunnel points at; empty to skip it")
	fs.StringVar(&o.upstream, "adb-upstream", fmt.Sprintf("127.0.0.1:%d", remote.DefaultADBPort),
		"the real adb server the proxy forwards to")
	fs.StringVar(&o.serial, "s", "", "USB serial of the bootloader to bridge, when several are attached")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Minute, "bound on a single USB transfer")
	fs.BoolVar(&o.startADB, "adb", true, "also make sure the local adb server is running")
	fs.BoolVar(&o.inject, "inject", true,
		"let the proxy explain states that would otherwise be an empty `adb devices`")
}

// startLocal brings up the bridge and the proxy. The ports are bound before it
// returns, so a port already in use is reported straight away rather than after
// a tunnel has been built around it. The returned channel carries the first
// failure from either component.
func startLocal(ctx context.Context, o localOpts, log *slog.Logger) (<-chan error, error) {
	if o.startADB {
		// adb listens on 127.0.0.1:5037 by default, which is what the proxy
		// forwards to, so there is nothing to configure -- just make sure it is
		// up before a remote client comes looking.
		if out, err := exec.Command("adb", "start-server").CombinedOutput(); err != nil {
			log.Warn("could not start the adb server", "err", err, "output", strings.TrimSpace(string(out)))
		} else {
			log.Info("adb server ready", "addr", o.upstream)
		}
	}

	fbLn, err := net.Listen("tcp", o.fastbootAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", o.fastbootAddr, err)
	}

	b := &fastboot.Bridge{Serial: o.serial, Timeout: o.timeout, Log: log}
	errs := make(chan error, 2)

	if o.proxyAddr != "" {
		pLn, err := net.Listen("tcp", o.proxyAddr)
		if err != nil {
			fbLn.Close()
			return nil, fmt.Errorf("listen on %s: %w", o.proxyAddr, err)
		}
		p := &adbproxy.Proxy{
			Upstream: o.upstream,
			Log:      log,
			Inject:   o.inject,
			// A device sitting in the bootloader is invisible to adb, which is
			// why a remote `adb devices` goes quiet at exactly the moment you
			// most want to know what happened to it.
			Bootloaders: func() []string {
				if b.InUse() {
					return nil // do not enumerate USB under an active flash
				}
				list, err := fastboot.List()
				if err != nil {
					return nil
				}
				out := make([]string, 0, len(list))
				for _, d := range list {
					out = append(out, d.Serial)
				}
				return out
			},
		}
		log.Info("adb proxy listening", "addr", pLn.Addr().String(), "upstream", o.upstream)
		go func() { errs <- p.Serve(ctx, pLn) }()
	}

	log.Info("fastboot bridge listening", "addr", fbLn.Addr().String())
	go func() { errs <- b.Serve(ctx, fbLn) }()
	return errs, nil
}

// serve runs only the local half. Use it when something other than radb -- a
// VPN, a hand-rolled tunnel, a systemd unit -- carries the ports.
func serve(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var o localOpts
	bindLocalFlags(fs, &o)
	verbose := fs.Bool("v", false, "log every fastboot command")
	fs.Parse(args)
	if *verbose {
		log = newLogger(true)
	}

	errs, err := startLocal(ctx, o, log)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		return err
	}
}

// link is the whole thing in one command: the local half plus the ssh reverse
// tunnel that carries it to the remote machine.
func link(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	var o localOpts
	bindLocalFlags(fs, &o)
	adbPort := fs.Int("adb-port", remote.DefaultADBPort, "port the remote side should use for adb")
	fbPort := fs.Int("fastboot-port", remote.DefaultFastbootPort, "port the remote side should use for fastboot")
	withServe := fs.Bool("serve", true, "also run the bridge and proxy here; -serve=false if they already run")
	verbose := fs.Bool("v", false, "log every fastboot command")
	fs.Parse(args)
	if *verbose {
		log = newLogger(true)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("link needs an ssh destination, e.g. radb link me@build-box")
	}

	// The remote keeps the ports its tools expect; locally they land on
	// whatever this process is actually listening on.
	adbLocal, err := portOf(o.proxyAddr)
	if err != nil {
		// With the proxy off, the tunnel has to reach the adb server itself.
		if adbLocal, err = portOf(o.upstream); err != nil {
			return fmt.Errorf("cannot tell which local port carries adb: %w", err)
		}
	}
	fbLocal, err := portOf(o.fastbootAddr)
	if err != nil {
		return fmt.Errorf("cannot tell which local port carries fastboot: %w", err)
	}

	errs := make(chan error, 2)
	if *withServe {
		local, err := startLocal(ctx, o, log)
		if err != nil {
			return err
		}
		go func() { errs <- <-local }()
	}

	t := &remote.Tunnel{
		Target: rest[0],
		Forwards: []remote.Forward{
			{Remote: *adbPort, Local: adbLocal},
			{Remote: *fbPort, Local: fbLocal},
		},
		Args: rest[1:], // anything further goes straight to ssh
		Log:  log,
	}
	tunnel := make(chan error, 1)
	go func() { tunnel <- t.Run(ctx) }()

	select {
	case err := <-errs:
		return err
	case err := <-tunnel:
		return err
	case <-ctx.Done():
	}

	// Shutting down: give ssh time to go away first. Exiting ahead of it
	// orphans it still holding the forwards, and the next link cannot bind
	// them until the server notices and lets go.
	select {
	case <-tunnel:
	case <-time.After(5 * time.Second):
		log.Warn("ssh has not exited; a forward may linger on the remote briefly")
	}
	return nil
}

// portOf pulls the port number out of a host:port address.
func portOf(addr string) (int, error) {
	if addr == "" {
		return 0, errors.New("no address given")
	}
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(p)
}

func devices() error {
	list, err := fastboot.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no devices in fastboot mode")
		return nil
	}
	for _, d := range list {
		fmt.Printf("%-24s usb:%s\n", d.Serial, d.Path)
	}
	return nil
}

func remoteEnv(args []string) error {
	fs := flag.NewFlagSet("remote-env", flag.ExitOnError)
	adbPort := fs.Int("adb-port", remote.DefaultADBPort, "adb server port on the remote side")
	fbPort := fs.Int("fastboot-port", remote.DefaultFastbootPort, "fastboot bridge port on the remote side")
	fs.Parse(args)

	fmt.Printf(`# radb: source this on the remote machine.

# adb's client speaks to a server over TCP, so this is all adb needs.
export ADB_SERVER_SOCKET=tcp:127.0.0.1:%d

# fastboot has no such variable; it takes the device on the command line.
# Either pass it yourself:
#     fastboot -s "$RADB_FASTBOOT" getvar product
# or copy radb/shim/rfastboot onto your PATH and use that instead:
#     rfastboot getvar product
export RADB_FASTBOOT=tcp:127.0.0.1:%d
`, *adbPort, *fbPort)
	return nil
}
