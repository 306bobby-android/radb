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
	"strconv"
	"strings"
	"sync"
	"time"
)

// Services worth treating specially.
const (
	svcKill     = "host:kill"
	svcDevices  = "host:devices"
	svcDevicesL = "host:devices-l"
	svcStatus   = "radb:status" // our own, for radb doctor
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

	mu      sync.Mutex
	kills   []incident
	version string
}

// incident records a client doing something we refused.
type incident struct {
	At      time.Time
	Peer    string
	Service string
}

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

	switch {
	case svc == svcKill:
		p.refuseKill(conn)
	case svc == svcStatus:
		writeOKAY(conn, []byte(p.Status()))
	case svc == svcDevices || svc == svcDevicesL:
		p.devices(conn, service, svc == svcDevicesL)
	default:
		p.passthrough(conn, service)
	}
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
	p.Log.Warn("refused host:kill", "peer", peer,
		"note", "usually an adb client whose server version differs from this one")

	writeFAIL(conn, fmt.Sprintf(
		"radb refused to kill this adb server: it is shared over a tunnel and other "+
			"sessions depend on it. Your adb disagrees with its version (this server reports %s), "+
			"so install platform-tools whose adb matches and retry.", p.serverVersion()))
}

// devices forwards the request and appends whatever synthetic entries apply.
func (p *Proxy) devices(conn net.Conn, service []byte, long bool) {
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

// passthrough splices the connection to the upstream server.
func (p *Proxy) passthrough(conn net.Conn, service []byte) {
	up, err := p.dial(conn)
	if err != nil {
		return
	}
	defer up.Close()
	if err := writeFrame(up, service); err != nil {
		return
	}
	go func() {
		io.Copy(up, conn)
		if tc, ok := up.(*net.TCPConn); ok {
			// Let the server see the client's EOF, which is how services like
			// push signal that they are done sending.
			tc.CloseWrite()
		}
	}()
	io.Copy(conn, up)
}

// dial reaches the upstream server, telling the client if it cannot.
func (p *Proxy) dial(conn net.Conn) (net.Conn, error) {
	up, err := net.DialTimeout("tcp", p.Upstream, 5*time.Second)
	if err != nil {
		p.Log.Warn("upstream unreachable", "addr", p.Upstream, "err", err)
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

// Status is the human readable report radb doctor asks for.
func (p *Proxy) Status() string {
	p.mu.Lock()
	kills := append([]incident(nil), p.kills...)
	p.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "upstream %s, server version %s\n", p.Upstream, p.serverVersion())
	if len(kills) == 0 {
		b.WriteString("no client has tried to kill this server\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d refused kill attempt(s) -- each one is an adb client whose\n", len(kills))
	fmt.Fprintf(&b, "server version differs from this server's (%s):\n", p.serverVersion())
	for _, k := range kills {
		fmt.Fprintf(&b, "  %s from %s\n", k.At.Format("2006-01-02 15:04:05"), k.Peer)
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
