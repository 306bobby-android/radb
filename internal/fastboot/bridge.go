package fastboot

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Bridge serves the fastboot-over-TCP protocol on a listener and relays each
// client to a bootloader attached over USB.
type Bridge struct {
	// Serial picks a USB device when more than one is in fastboot mode.
	Serial string
	// Timeout bounds a single USB transfer.
	Timeout time.Duration
	Log     *slog.Logger

	// usb serialises clients. The bulk endpoints can only be claimed by one
	// session, and fastboot is strictly one command at a time anyway.
	usb sync.Mutex
}

// Serve accepts clients until ctx is cancelled or the listener fails.
func (b *Bridge) Serve(ctx context.Context, ln net.Listener) error {
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
		go b.handle(ctx, conn)
	}
}

// handle runs one client from handshake to disconnect.
func (b *Bridge) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	log := b.Log.With("peer", conn.RemoteAddr().String())

	if tc, ok := conn.(*net.TCPConn); ok {
		// Every command is a small write followed by a read, so Nagle would add
		// a round trip of latency to each one across the tunnel.
		tc.SetNoDelay(true)
	}

	if _, err := Handshake(conn); err != nil {
		log.Warn("handshake failed", "err", err)
		return
	}

	b.usb.Lock()
	defer b.usb.Unlock()

	dev, err := Open(b.Serial)
	if err != nil {
		// Stay on the wire and answer FAIL rather than dropping the connection:
		// fastboot then prints the reason instead of a bare reset, which is the
		// difference between a usable error and a mystery.
		log.Warn("no bootloader to bridge to", "err", err)
		serveNoDevice(conn, err.Error())
		return
	}
	dev.Timeout = b.Timeout
	defer dev.Close()
	log.Info("bridging", "serial", dev.Serial, "usb", dev.Path)

	s := NewSession(dev, NewPacketReader(bufio.NewReaderSize(conn, 256<<10)), NewPacketWriter(conn), log)
	if err := s.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Warn("session ended", "err", err)
		return
	}
	log.Info("client disconnected", "serial", dev.Serial)
}

// serveNoDevice answers every command with FAIL so the operator is told why the
// bridge could not help.
func serveNoDevice(conn net.Conn, reason string) {
	r := NewPacketReader(bufio.NewReader(conn))
	w := NewPacketWriter(conn)
	msg := []byte(respFAIL + reason)
	if len(msg) > respMax {
		msg = msg[:respMax]
	}
	for {
		if _, err := r.Next(); err != nil {
			return
		}
		if _, err := r.Full(); err != nil {
			return
		}
		if err := w.Send(msg); err != nil {
			return
		}
	}
}
