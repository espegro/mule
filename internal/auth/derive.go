package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

type Role string

const (
	RoleForward Role = "forward"
	RoleExit    Role = "exit"
)

const hkdfSalt = "mule/v1/auth"

func deriveBytes(secret []byte, label string, n int) ([]byte, error) {
	out := make([]byte, n)
	r := hkdf.New(sha256.New, secret, []byte(hkdfSalt), []byte(label))
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}

func derivePrivateKey(secret []byte, label string) (ed25519.PrivateKey, error) {
	seed, err := deriveBytes(secret, label, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func roleIdentityLabel(role Role) (string, error) {
	switch role {
	case RoleForward:
		return "mule/v1/forward identity", nil
	case RoleExit:
		return "mule/v1/exit identity", nil
	default:
		return "", fmt.Errorf("unknown role %q", role)
	}
}

func RolePublicKey(secret []byte, role Role) (ed25519.PublicKey, error) {
	label, err := roleIdentityLabel(role)
	if err != nil {
		return nil, err
	}
	priv, err := derivePrivateKey(secret, label)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	out := make([]byte, len(pub))
	copy(out, pub)
	return out, nil
}

func IdentitiesDiffer(secret []byte) (bool, error) {
	forwardPub, err := RolePublicKey(secret, RoleForward)
	if err != nil {
		return false, err
	}
	exitPub, err := RolePublicKey(secret, RoleExit)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(forwardPub, exitPub), nil
}
