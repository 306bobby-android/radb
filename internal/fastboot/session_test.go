package fastboot

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// fakeBoot is a scripted bootloader: it records everything written to the bulk
// OUT endpoint and answers bulk IN reads from a queue.
type fakeBoot struct {
	sent    bytes.Buffer
	replies [][]byte
	pkt     int
}

func (f *fakeBoot) Send(_ context.Context, b []byte) error {
	f.sent.Write(b)
	return nil
}

func (f *fakeBoot) Recv(_ context.Context, b []byte) (int, error) {
	if len(f.replies) == 0 {
		return 0, io.EOF
	}
	r := f.replies[0]
	n := copy(b, r)
	if n < len(r) {
		f.replies[0] = r[n:]
	} else {
		f.replies = f.replies[1:]
	}
	return n, nil
}

func (f *fakeBoot) InPacketSize() int {
	if f.pkt == 0 {
		return 512
	}
	return f.pkt
}

// pkt frames b the way a fastboot client would.
func pkt(b []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(len(b)))
	return append(hdr[:], b...)
}

// unpkt splits a bridge output stream back into packets.
func unpkt(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("trailing %d bytes are too short for a header", len(b))
		}
		n := binary.BigEndian.Uint64(b[:8])
		b = b[8:]
		if uint64(len(b)) < n {
			t.Fatalf("header promised %d bytes but only %d remain", n, len(b))
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

// run drives a session over a canned client stream and returns what the client
// would have seen plus what reached the bootloader.
func run(t *testing.T, boot *fakeBoot, client []byte) ([][]byte, []byte, error) {
	t.Helper()
	var wire bytes.Buffer
	s := NewSession(boot,
		NewPacketReader(bytes.NewReader(client)),
		NewPacketWriter(&wire),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := s.Serve(context.Background())
	return unpkt(t, wire.Bytes()), boot.sent.Bytes(), err
}

func TestSimpleCommand(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("OKAYredfin")}}
	got, sent, err := run(t, boot, pkt([]byte("getvar:product")))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if string(sent) != "getvar:product" {
		t.Errorf("bootloader got %q", sent)
	}
	if len(got) != 1 || string(got[0]) != "OKAYredfin" {
		t.Errorf("client saw %q", got)
	}
}

func TestInfoLinesAreRelayedBeforeTheResult(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{
		[]byte("INFOerasing"),
		[]byte("INFOdone"),
		[]byte("OKAY"),
	}}
	got, _, err := run(t, boot, pkt([]byte("erase:cache")))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := []string{"INFOerasing", "INFOdone", "OKAY"}
	if len(got) != len(want) {
		t.Fatalf("got %d packets, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("packet %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A download payload may arrive split across packets, because the host writes
// it in whatever chunks it happens to read.
func TestDownloadReassemblesSplitPayload(t *testing.T) {
	payload := bytes.Repeat([]byte("ab"), 8) // 16 bytes
	boot := &fakeBoot{replies: [][]byte{
		[]byte("DATA00000010"),
		[]byte("OKAY"),
	}}

	var client bytes.Buffer
	client.Write(pkt([]byte("download:00000010")))
	client.Write(pkt(payload[:5]))
	client.Write(pkt(payload[5:]))

	got, sent, err := run(t, boot, client.Bytes())
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if want := "download:00000010" + string(payload); string(sent) != want {
		t.Errorf("bootloader got %q, want %q", sent, want)
	}
	if len(got) != 2 || string(got[0]) != "DATA00000010" || string(got[1]) != "OKAY" {
		t.Errorf("client saw %q", got)
	}
}

// The invariant that makes `fetch` work: however many USB transfers the payload
// arrives in, it must leave as exactly one packet. The host reads it in chunks
// of up to 1 MiB and treats a short read as fatal, and its TCP transport never
// reads across a packet boundary.
func TestUploadLeavesAsASinglePacket(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 4096)
	boot := &fakeBoot{replies: [][]byte{
		[]byte("DATA00001000"),
		payload[:1000], // three short transfers, as a real endpoint would give
		payload[1000:3000],
		payload[3000:],
		[]byte("OKAY"),
	}}

	got, _, err := run(t, boot, pkt([]byte("fetch:boot_a:0:1000")))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3 (DATA, payload, OKAY)", len(got))
	}
	if string(got[0]) != "DATA00001000" {
		t.Errorf("first packet = %q", got[0])
	}
	if !bytes.Equal(got[1], payload) {
		t.Errorf("payload packet is %d bytes, want %d", len(got[1]), len(payload))
	}
	if string(got[2]) != "OKAY" {
		t.Errorf("last packet = %q", got[2])
	}
}

// A bootloader that sends more than the DATA header promised must not be
// allowed to push the extra bytes into the packet and desynchronise the stream.
func TestUploadIgnoresOvershoot(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{
		[]byte("DATA00000004"),
		[]byte("abcdefgh"),
		[]byte("OKAY"),
	}}
	got, _, err := run(t, boot, pkt([]byte("upload")))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) < 2 || string(got[1]) != "abcd" {
		t.Errorf("payload packet = %q, want %q", got[1], "abcd")
	}
}

func TestFailIsTerminal(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("FAILunknown command")}}
	got, _, err := run(t, boot, pkt([]byte("nonsense")))
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 1 || !strings.HasPrefix(string(got[0]), "FAIL") {
		t.Errorf("client saw %q", got)
	}
}

// If a command we do not know asks for a data phase we cannot guess which way
// the bytes flow, and guessing wrong would corrupt the stream.
func TestUnknownDataDirectionIsAnError(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("DATA00000004")}}
	_, _, err := run(t, boot, pkt([]byte("oem weird")))
	if err == nil {
		t.Fatal("want an error for a data phase after an unknown command")
	}
	if !strings.Contains(err.Error(), "data direction") {
		t.Errorf("error = %v", err)
	}
}

func TestGarbageResponseIsRejected(t *testing.T) {
	boot := &fakeBoot{replies: [][]byte{[]byte("what")}}
	_, _, err := run(t, boot, pkt([]byte("getvar:product")))
	if err == nil {
		t.Fatal("want an error for a response with no known tag")
	}
}

func TestOversizedCommandIsRejected(t *testing.T) {
	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], 1<<40)
	_, _, err := run(t, &fakeBoot{}, hdr[:])
	if err == nil || !strings.Contains(err.Error(), "out of sync") {
		t.Fatalf("err = %v, want an out-of-sync complaint", err)
	}
}

func TestHandshake(t *testing.T) {
	// A peer that speaks our version: we should read its number back.
	peer := &rw{r: strings.NewReader("FB01")}
	v, err := Handshake(peer)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v != 1 {
		t.Errorf("peer version = %d, want 1", v)
	}
	if peer.w.String() != fmt.Sprintf("FB%02d", ProtocolVersion) {
		t.Errorf("we sent %q", peer.w.String())
	}
}

func TestHandshakeRejectsNonFastboot(t *testing.T) {
	if _, err := Handshake(&rw{r: strings.NewReader("GET ")}); err == nil {
		t.Fatal("want an error for a non-fastboot peer")
	}
}

type rw struct {
	r io.Reader
	w bytes.Buffer
}

func (x *rw) Read(b []byte) (int, error)  { return x.r.Read(b) }
func (x *rw) Write(b []byte) (int, error) { return x.w.Write(b) }
