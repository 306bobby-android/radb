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
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bobbypanarisi/radb/internal/adbproxy"
	"github.com/bobbypanarisi/radb/internal/fastboot"
	"github.com/bobbypanarisi/radb/internal/remote"
)

const usage = `radb lends a USB-attached Android device to a remote machine.

Run on the machine with the device plugged in:
  radb serve                 serve the fastboot bridge and start the adb server
  radb link USER@HOST        hold an ssh reverse tunnel open to the remote box
  radb devices               list bootloaders visible on USB
  radb doctor                check both halves of the setup

Run anywhere:
  radb remote-env            print the shell setup for the remote machine

Use "radb COMMAND -h" for the flags of a command.
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:], log)
	case "link":
		err = link(ctx, os.Args[2:], log)
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

func serve(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", fmt.Sprintf("127.0.0.1:%d", remote.DefaultFastbootPort),
		"address for the fastboot bridge; loopback is right when an ssh tunnel carries it")
	serial := fs.String("s", "", "USB serial of the bootloader to bridge, when several are attached")
	timeout := fs.Duration("timeout", 10*time.Minute, "bound on a single USB transfer")
	startADB := fs.Bool("adb", true, "also make sure the local adb server is running")
	proxyAddr := fs.String("adb-proxy", fmt.Sprintf("127.0.0.1:%d", remote.DefaultProxyPort),
		"address for the adb proxy that the tunnel should point at; empty to disable it")
	upstream := fs.String("adb-upstream", fmt.Sprintf("127.0.0.1:%d", remote.DefaultADBPort),
		"the real adb server the proxy forwards to")
	inject := fs.Bool("inject", true,
		"let the proxy add explanatory entries to `adb devices` for states that would otherwise be an empty list")
	verbose := fs.Bool("v", false, "log every fastboot command")
	fs.Parse(args)

	if *verbose {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	if *startADB {
		// adb listens on 127.0.0.1:5037 by default, which is exactly what the
		// tunnel forwards, so there is nothing to configure -- just make sure
		// it is up before the remote client tries to reach it.
		if out, err := exec.Command("adb", "start-server").CombinedOutput(); err != nil {
			log.Warn("could not start the adb server", "err", err, "output", strings.TrimSpace(string(out)))
		} else {
			log.Info("adb server ready", "addr", fmt.Sprintf("127.0.0.1:%d", remote.DefaultADBPort))
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	defer ln.Close()
	log.Info("fastboot bridge listening", "addr", ln.Addr().String())

	b := &fastboot.Bridge{Serial: *serial, Timeout: *timeout, Log: log}

	if *proxyAddr != "" {
		pln, err := net.Listen("tcp", *proxyAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", *proxyAddr, err)
		}
		defer pln.Close()
		p := &adbproxy.Proxy{
			Upstream: *upstream,
			Log:      log,
			Inject:   *inject,
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
		log.Info("adb proxy listening", "addr", pln.Addr().String(), "upstream", *upstream)
		go func() {
			if err := p.Serve(ctx, pln); err != nil {
				log.Error("adb proxy stopped", "err", err)
			}
		}()
	}

	return b.Serve(ctx, ln)
}

func link(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	adbPort := fs.Int("adb-port", remote.DefaultADBPort, "port the remote side should use for adb")
	fbPort := fs.Int("fastboot-port", remote.DefaultFastbootPort, "port the remote side should use for fastboot")
	proxyPort := fs.Int("adb-proxy-port", remote.DefaultProxyPort,
		"local port the adb traffic lands on; 5038 is radb's proxy, 5037 the bare adb server")
	fbLocal := fs.Int("fastboot-local-port", remote.DefaultFastbootPort,
		"local port the fastboot bridge listens on")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("link needs an ssh destination, e.g. radb link me@build-box")
	}

	t := &remote.Tunnel{
		Target: rest[0],
		Forwards: []remote.Forward{
			// The remote keeps adb's usual port; locally it lands on the proxy.
			{Remote: *adbPort, Local: *proxyPort},
			{Remote: *fbPort, Local: *fbLocal},
		},
		Args: rest[1:], // anything further goes straight to ssh
		Log:  log,
	}
	return t.Run(ctx)
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
