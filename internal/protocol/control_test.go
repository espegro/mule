package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Type: TypeError, Code: ErrorDialFailed}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestOpenFrameRoundTripWithRoute(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{
		Type:         TypeOpen,
		Route:        "web",
		ForwardID:    "host-b",
		Listener:     "127.0.0.1:3000",
		SourceAddr:   "192.0.2.10:53144",
		ConnectionID: "abc123",
	}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestFrameRejectsBadVersion(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0, 2, 99, byte(TypeOpen)})
	_, err := ReadFrame(buf)
	if !errors.Is(err, ErrBadVersion) {
		t.Fatalf("got %v, want %v", err, ErrBadVersion)
	}
}

func TestFrameRejectsTooLarge(t *testing.T) {
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], MaxFrameLen+1)
	_, err := ReadFrame(bytes.NewReader(hdr[:]))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want %v", err, ErrFrameTooLarge)
	}
}

func TestFrameRejectsExtraBytes(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0, 3, Version, byte(TypeOpen), 0})
	_, err := ReadFrame(buf)
	if !errors.Is(err, ErrBadFrame) {
		t.Fatalf("got %v, want %v", err, ErrBadFrame)
	}
}
