package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Version     = 2
	MaxFrameLen = 4096
)

type Type byte

const (
	TypeOpen  Type = 1
	TypeOK    Type = 2
	TypeError Type = 3
	TypeProbe Type = 4
)

type ErrorCode byte

const (
	ErrorDialFailed    ErrorCode = 1
	ErrorOverloaded    ErrorCode = 2
	ErrorUnauthorized  ErrorCode = 3
	ErrorInternalError ErrorCode = 4
)

type Direction byte

const (
	DirectionForward Direction = 1
	DirectionReverse Direction = 2
)

type Frame struct {
	Type         Type
	Code         ErrorCode
	Direction    Direction
	Service      string
	PeerID       string
	Listener     string
	SourceAddr   string
	ConnectionID string
}

var (
	ErrFrameTooLarge = errors.New("control frame too large")
	ErrBadVersion    = errors.New("unsupported control protocol version")
	ErrBadFrame      = errors.New("invalid control frame")
)

func WriteFrame(w io.Writer, f Frame) error {
	body := []byte{Version, byte(f.Type)}
	switch f.Type {
	case TypeOpen, TypeProbe:
		if f.Direction != DirectionForward && f.Direction != DirectionReverse {
			return ErrBadFrame
		}
		body = append(body, byte(f.Direction))
		var err error
		body, err = appendString(body, f.Service)
		if err != nil {
			return ErrFrameTooLarge
		}
		for _, s := range []string{f.PeerID, f.Listener, f.SourceAddr, f.ConnectionID} {
			body, err = appendString(body, s)
			if err != nil {
				return ErrFrameTooLarge
			}
		}
	case TypeError:
		body = append(body, byte(f.Code))
	}
	if len(body) > MaxFrameLen {
		return ErrFrameTooLarge
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > MaxFrameLen {
		return Frame{}, ErrFrameTooLarge
	}
	if n < 2 {
		return Frame{}, ErrBadFrame
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	if body[0] != Version {
		return Frame{}, ErrBadVersion
	}
	f := Frame{Type: Type(body[1])}
	switch f.Type {
	case TypeOpen, TypeProbe:
		if len(body) < 3 {
			return Frame{}, ErrBadFrame
		}
		f.Direction = Direction(body[2])
		if f.Direction != DirectionForward && f.Direction != DirectionReverse {
			return Frame{}, ErrBadFrame
		}
		rest := body[3:]
		var err error
		f.Service, rest, err = readString(rest)
		if err != nil {
			return Frame{}, err
		}
		f.PeerID, rest, err = readString(rest)
		if err != nil {
			return Frame{}, err
		}
		f.Listener, rest, err = readString(rest)
		if err != nil {
			return Frame{}, err
		}
		f.SourceAddr, rest, err = readString(rest)
		if err != nil {
			return Frame{}, err
		}
		f.ConnectionID, rest, err = readString(rest)
		if err != nil {
			return Frame{}, err
		}
		if len(rest) != 0 {
			return Frame{}, ErrBadFrame
		}
	case TypeOK:
		if len(body) != 2 {
			return Frame{}, ErrBadFrame
		}
	case TypeError:
		if len(body) != 3 {
			return Frame{}, ErrBadFrame
		}
		f.Code = ErrorCode(body[2])
		if !validErrorCode(f.Code) {
			return Frame{}, ErrBadFrame
		}
	default:
		return Frame{}, fmt.Errorf("%w: unknown type %d", ErrBadFrame, f.Type)
	}
	return f, nil
}

func appendString(body []byte, s string) ([]byte, error) {
	if len(s) > MaxFrameLen {
		return nil, ErrFrameTooLarge
	}
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
	body = append(body, lenBuf[:]...)
	body = append(body, []byte(s)...)
	if len(body) > MaxFrameLen {
		return nil, ErrFrameTooLarge
	}
	return body, nil
}

func readString(body []byte) (string, []byte, error) {
	if len(body) < 2 {
		return "", nil, ErrBadFrame
	}
	n := int(binary.BigEndian.Uint16(body[:2]))
	body = body[2:]
	if n > len(body) {
		return "", nil, ErrBadFrame
	}
	return string(body[:n]), body[n:], nil
}

func validErrorCode(code ErrorCode) bool {
	switch code {
	case ErrorDialFailed, ErrorOverloaded, ErrorUnauthorized, ErrorInternalError:
		return true
	default:
		return false
	}
}
