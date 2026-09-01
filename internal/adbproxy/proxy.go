// Package adbproxy fronts a local adb server so that clients reaching it over a
// tunnel cannot damage it, and so that failures which would otherwise show up
// as an empty device list explain themselves.
//
// The adb wire format is simple enough to sit in the middle of: a client opens a
// connection and writes a four hex digit length followed by a service name; the
// server answers OKAY or FAIL and, for most services, one more length-prefixed
// frame. Everything except the services named below is spliced straight through.
package adbproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/306bobby-android/radb/internal/activity"
)

// Services worth treating specially.
const (
	svcKill     = "host:kill"
	svcDevices  = "host:devices"
	svcDevicesL = "host:devices-l"
	svcStatus   = "radb:status"   // our own, for radb doctor
	svcShutdown = "radb:shutdown" // our own, to stop radb from the remote side

	// ShutdownHost is the pseudo-host that makes `adb connect` a shutdown
	// button. `adb connect` puts its argument on the wire untouched and prints
	// the answer, which makes it the only stock adb command that can carry a
	// word of our choosing to the proxy and show the reply.
	ShutdownHost = "radb-shutdown"
)

// Proxy relays adb clients to a real adb server.
type Proxy struct {
	// Upstream is the real adb server, normally 127.0.0.1:5037.
	Upstream string
	Log      *slog.Logger

	// Inject adds synthetic entries to the device list to explain states that
	// would otherwise be an empty list.
	Inject bool

	// Bootloaders reports the serials of devices sitting in fastboot mode,
	// which adb cannot see at all. May be nil.
	Bootloaders func() []string

	// Idle is the inactivity timeout radb was started with, reported by Status
	// so that a session which stopped on its own is not a mystery. Zero is no
	// timeout.
	Idle time.Duration

	// Shutdown, when set, lets a remote client stop radb. Reaching this port
	// already means being able to drive the device, so it grants nothing new;
	// it is a field rather than a constant so the caller can withhold it.
	Shutdown func(reason string)

	act activity.Tracker

	mu      sync.Mutex
	kills   []incident
	version string
	devs    map[string]string // serial -> state, from host:track-devices
	tracked bool
}

// incident records a client doing something we refused.
type incident struct {
	At      time.Time
	Peer    string
	Service string
}

func (p *Proxy) log() *slog.Logger {
	if p.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return p.Log
}

// Busy reports whether a client is connected right now.
func (p *Proxy) Busy() bool { return p.act.Busy() }

// LastActivity is when a client was most recently served.
func (p *Proxy) LastActivity() time.Time { return p.act.Last() }

// Serve accepts clients until ctx is cancelled or the listener fails.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go p.handle(ctx, conn)
	}
}

func (p *Proxy) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	service, err := readFrame(conn)
	if err != nil {
		return
	}
	svc := string(service)

	// Asking after radb is not using it. Counting doctor as a client would let
	// anything that polls for health hold the idle timeout open indefinitely.
	if svc != svcStatus {
		done := p.act.Begin()
		defer done()
	}

	switch {
	case svc == svcKill:
		p.refuseKill(conn)
	case svc == svcStatus:
		writeOKAY(conn, []byte(p.Status()))
	case svc == svcShutdown || isShutdownRequest(svc):
		p.shutdown(conn, svc)
	case svc == svcDevices || svc == svcDevicesL:
		p.devices(conn, service, svc == svcDevicesL)
	default:
		p.passthrough(conn, service)
	}
}

// isShutdownRequest spots `adb connect radb-shutdown` and its disconnect twin.
// The server would normally append a default port to a hostname with none, so
// accept the address either way.
func isShutdownRequest(svc string) bool {
	host, ok := strings.CutPrefix(svc, "host:connect:")
	if !ok {
		if host, ok = strings.CutPrefix(svc, "host:disconnect:"); !ok {
			return false
		}
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == ShutdownHost
}

// shutdown stops radb at a remote client's request.
func (p *Proxy) shutdown(conn net.Conn, svc string) {
	peer := conn.RemoteAddr().String()
	if p.Shutdown == nil {
		p.log().Warn("refused a remote shutdown", "peer", peer, "service", svc)
		writeOKAY(conn, []byte("failed to connect to "+ShutdownHost+": radb was started with "+
			"-remote-shutdown=false, so it will not stop at a client's request."))
		return
	}
	p.log().Warn("shutting down at a client's request", "peer", peer, "service", svc)
	// `adb connect` exits non-zero unless the answer opens with "connected
	// to", and a shutdown that worked should not look like a failed one.
	writeOKAY(conn, []byte("connected to "+ShutdownHost+": radb is stopping. The tunnel and "+
		"both bridges go with it; run radb link again on the device host to bring them back."))
	// Answer before unwinding: once the context is cancelled this connection is
	// on its way out and the client would be left with a bare reset. The pause
	// is for the ssh that carried the request, which is about to be signalled
	// and has to forward the reply first.
	conn.Close()
	time.Sleep(250 * time.Millisecond)
	p.Shutdown("a client asked for it from " + peer)
}

// refuseKill declines to pass on host:kill.
//
// An adb client whose version disagrees with the server's kills the server and
// starts its own. Across a tunnel that is doubly wrong: it cannot start a
// replacement, and it takes down the server every other session is using. The
// client prints a FAIL message verbatim as "error: ...", which makes this the
// one place we can hand the operator an explanation at the moment it breaks.
func (p *Proxy) refuseKill(conn net.Conn) {
	peer := conn.RemoteAddr().String()
	p.mu.Lock()
	p.kills = append(p.kills, incident{At: time.Now(), Peer: peer, Service: svcKill})
	if len(p.kills) > 16 {
		p.kills = p.kills[len(p.kills)-16:]
	}
	p.mu.Unlock()
	p.log().Warn("refused host:kill", "peer", peer,
		"note", "usually an adb client whose server version differs from this one")

	writeFAIL(conn, fmt.Sprintf(
		"radb refused to kill this adb server: it is shared over a tunnel and other "+
			"sessions depend on it. Your adb disagrees with its version (this server reports %s), "+
			"so install platform-tools whose adb matches and retry.", p.serverVersion()))
}

// devices forwards the request and appends whatever synthetic entries apply.
func (p *Proxy) devices(conn net.Conn, service []byte, long bool) {
	log := p.log().With("peer", conn.RemoteAddr().String(), "service", string(service))
	up, err := p.dial(conn)
	if err != nil {
		return
	}
	defer up.Close()

	if err := writeFrame(up, service); err != nil {
		return
	}
	var status [4]byte
	if _, err := io.ReadFull(up, status[:]); err != nil {
		return
	}
	payload, err := readFrame(up)
	if err != nil {
		return
	}
	log.Info("adb", "status", string(status[:]))
	if string(status[:]) != "OKAY" {
		// Pass a failure along untouched; it is the server's to explain.
		conn.Write(status[:])
		writeFrame(conn, payload)
		return
	}
	if p.Inject {
		payload = append(payload, []byte(p.render(long))...)
	}
	writeOKAY(conn, payload)
}

// render formats the synthetic entries. Both columns are free text that the adb
// client prints verbatim, so the state column carries the explanation.
func (p *Proxy) render(long bool) string {
	var b strings.Builder
	for _, e := range p.entries() {
		if long {
			fmt.Fprintf(&b, "%-24s %s transport_id:0\n", e.serial, e.state)
		} else {
			fmt.Fprintf(&b, "%s\t%s\n", e.serial, e.state)
		}
	}
	return b.String()
}

type entry struct{ serial, state string }

// entries lists the conditions worth reporting through the device list.
func (p *Proxy) entries() []entry {
	var out []entry

	p.mu.Lock()
	last := len(p.kills)
	var at time.Time
	if last > 0 {
		at = p.kills[last-1].At
	}
	p.mu.Unlock()

	if last > 0 {
		out = append(out, entry{
			serial: "radb-ADB-VERSION-MISMATCH",
			state: fmt.Sprintf("a-client-tried-to-kill-this-v%s-server-at-%s",
				p.serverVersion(), at.Format("15:04:05")),
		})
	}

	if p.Bootloaders != nil {
		for _, s := range p.Bootloaders() {
			out = append(out, entry{serial: s, state: "in-fastboot-mode-use-fastboot-not-adb"})
		}
	}
	return out
}

// quiet marks the services an adb client sends as bookkeeping before the
// command you actually typed. Logging them buries the real ones.
func quiet(svc string) bool {
	switch svc {
	case "host:version", "host:features", "host-features", "host:host-features":
		return true
	}
	return false
}

// passthrough splices the connection to the upstream server, reporting what was
// asked for and what came back.
//
// The first reply is always a four byte OKAY or FAIL, so it can be read out of
// the stream and logged before the rest is spliced. A transport switch is worth
// following one step further: `host:transport-any` says nothing about what the
// client is doing, and the service that follows it on the same connection --
// shell:, sync:, reboot: -- is the command itself.
func (p *Proxy) passthrough(conn net.Conn, service []byte) {
	svc := string(service)
	log := p.log().With("peer", conn.RemoteAddr().String(), "service", clip(svc))
	report := log.Info
	if quiet(svc) {
		report = log.Debug
	}

	up, err := p.dial(conn)
	if err != nil {
		return
	}
	defer up.Close()
	if err := writeFrame(up, service); err != nil {
		return
	}

	var status [4]byte
	if _, err := io.ReadFull(up, status[:]); err != nil {
		log.Warn("adb", "err", "the server closed without answering")
		return
	}
	if _, err := conn.Write(status[:]); err != nil {
		return
	}
	if string(status[:]) != "OKAY" {
		reason, err := readFrame(up)
		if err != nil {
			return
		}
		report("adb", "status", "FAIL", "reason", clip(string(reason)))
		writeFrame(conn, reason)
		return
	}

	var client io.Reader = conn
	var server io.Reader = up
	if isTransport(svc) {
		// The command lands on the client's side of the wire and its verdict
		// comes back on the server's, so both are sniffed and reported
		// together. Sniffing leaves the bytes themselves alone.
		var ex exchange
		client = &sniffer{r: conn, want: sniffFrame(ex.asked)}
		server = &sniffer{r: up, want: sniffSkip(transportPad(svc), sniffStatus(func(st string) {
			report("adb", "command", ex.command(), "status", st)
		}))}
	} else {
		report("adb", "status", "OKAY")
	}

	go func() {
		io.Copy(up, client)
		if tc, ok := up.(*net.TCPConn); ok {
			// Let the server see the client's EOF, which is how services like
			// push signal that they are done sending.
			tc.CloseWrite()
		}
	}()
	io.Copy(conn, server)
}

// exchange holds what a client asked a device to do until the answer arrives,
// so that the two reach the log as one line.
type exchange struct {
	mu  sync.Mutex
	cmd string
}

func (e *exchange) asked(cmd string) {
	e.mu.Lock()
	e.cmd = clip(tidy(cmd))
	e.mu.Unlock()
}

func (e *exchange) command() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cmd == "" {
		return "?"
	}
	return e.cmd
}

// tidy drops the options a service carries between its name and its argument --
// shell,v2,TERM=xterm-256color,raw:ls. They are protocol negotiation, and they
// crowd out the part of the line worth reading.
func tidy(svc string) string {
	name, arg, ok := strings.Cut(svc, ":")
	if !ok {
		return svc
	}
	if base, _, hasOpts := strings.Cut(name, ","); hasOpts {
		return base + ":" + arg
	}
	return svc
}

// isTransport spots the services that hand the connection to a device, after
// which the client sends the service it really came for. Current adb clients
// ask for tport: and older ones for transport:; both are still served.
func isTransport(svc string) bool {
	if !strings.HasPrefix(svc, "host") {
		return false
	}
	return strings.Contains(svc, ":transport") || strings.Contains(svc, ":tport:")
}

// transportPad is how many bytes the server puts between its OKAY and the
// answer to the next service. tport: is followed by a raw 64 bit transport id;
// the older transport: forms are followed by nothing.
func transportPad(svc string) int {
	if strings.Contains(svc, ":tport:") {
		return 8
	}
	return 0
}

// dial reaches the upstream server, telling the client if it cannot.
func (p *Proxy) dial(conn net.Conn) (net.Conn, error) {
	up, err := net.DialTimeout("tcp", p.Upstream, 5*time.Second)
	if err != nil {
		p.log().Warn("upstream unreachable", "addr", p.Upstream, "err", err)
		writeFAIL(conn, fmt.Sprintf("radb cannot reach the adb server at %s: %v "+
			"(is it running? try: adb start-server)", p.Upstream, err))
		return nil, err
	}
	return up, nil
}

// serverVersion asks the upstream what version it reports, once.
func (p *Proxy) serverVersion() string {
	p.mu.Lock()
	if p.version != "" {
		v := p.version
		p.mu.Unlock()
		return v
	}
	p.mu.Unlock()

	v := "unknown"
	if up, err := net.DialTimeout("tcp", p.Upstream, 2*time.Second); err == nil {
		defer up.Close()
		up.SetDeadline(time.Now().Add(2 * time.Second))
		if writeFrame(up, []byte("host:version")) == nil {
			var status [4]byte
			if _, err := io.ReadFull(up, status[:]); err == nil && string(status[:]) == "OKAY" {
				if b, err := readFrame(up); err == nil {
					if n, err := strconv.ParseUint(string(b), 16, 32); err == nil {
						v = strconv.FormatUint(n, 10)
					}
				}
			}
		}
	}
	p.mu.Lock()
	p.version = v
	p.mu.Unlock()
	return v
}

// Status is the human readable report radb doctor asks for. A line starting
// with "! " is one doctor should draw attention to.
func (p *Proxy) Status() string {
	p.mu.Lock()
	kills := append([]incident(nil), p.kills...)
	devs := p.devs
	p.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "upstream %s, server version %s\n", p.Upstream, p.serverVersion())

	if len(devs) == 0 {
		b.WriteString("! adb can see no device\n")
	} else {
		for _, serial := range sortedKeys(devs) {
			fmt.Fprintf(&b, "adb device %s (%s)\n", serial, devs[serial])
		}
	}

	if last := p.act.Last(); last.IsZero() {
		b.WriteString("no client has connected yet\n")
	} else {
		fmt.Fprintf(&b, "last client %s ago\n", time.Since(last).Round(time.Second))
	}
	if p.Idle > 0 {
		fmt.Fprintf(&b, "will exit after %s idle or with no device\n", p.Idle)
	}
	if p.Shutdown != nil {
		fmt.Fprintf(&b, "the remote side can stop radb with: adb connect %s\n", ShutdownHost)
	}

	if len(kills) == 0 {
		b.WriteString("no client has tried to kill this server\n")
		return b.String()
	}
	fmt.Fprintf(&b, "! %d refused kill attempt(s) -- each one is an adb client whose\n", len(kills))
	fmt.Fprintf(&b, "! server version differs from this server's (%s):\n", p.serverVersion())
	for _, k := range kills {
		fmt.Fprintf(&b, "!   %s from %s\n", k.At.Format("2006-01-02 15:04:05"), k.Peer)
	}
	return b.String()
}

// readFrame reads a four hex digit length followed by that many bytes.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		return nil, fmt.Errorf("bad frame length %q: %w", hdr[:], err)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func writeFrame(w io.Writer, b []byte) error {
	if _, err := fmt.Fprintf(w, "%04x", len(b)); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func writeOKAY(w io.Writer, b []byte) error {
	if _, err := io.WriteString(w, "OKAY"); err != nil {
		return err
	}
	return writeFrame(w, b)
}

func writeFAIL(w io.Writer, msg string) error {
	if _, err := io.WriteString(w, "FAIL"); err != nil {
		return err
	}
	return writeFrame(w, []byte(msg))
}

// clip renders wire text for a log line without letting a long argument -- an
// oem passthrough, a shell one-liner -- run away with the line.
func clip(s string) string {
	const limit = 120
	s = strings.TrimRight(s, "\x00")
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// sortedKeys is a small convenience for reporting maps in a stable order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
