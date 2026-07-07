package auth

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDecodeSecretBase64(t *testing.T) {
	raw := make([]byte, MinSecretBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	got, err := DecodeSecret(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal("decoded secret mismatch")
	}
}

func TestDecodeSecretHex(t *testing.T) {
	raw := make([]byte, MinSecretBytes)
	for i := range raw {
		raw[i] = byte(255 - i)
	}
	got, err := DecodeSecret(hex.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal("decoded secret mismatch")
	}
}

func TestDecodeSecretRejectsEmptyShortAndInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{name: "empty", in: "", want: ErrSecretTooShort},
		{name: "short", in: base64.StdEncoding.EncodeToString([]byte("short")), want: ErrSecretTooShort},
		{name: "invalid", in: "not valid secret material", want: ErrSecretEncoding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeSecret(tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLoadSecretFileRejectsOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions check does not apply")
	}
	path := filepath.Join(t.TempDir(), "key")
	raw := make([]byte, MinSecretBytes)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSecretFile(path)
	if err == nil {
		t.Fatal("expected permission error")
	}
}

func TestDerivedIdentitiesAreDeterministicAndDifferent(t *testing.T) {
	secret := make([]byte, MinSecretBytes)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	a, err := RolePublicKey(secret, RoleForward)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RolePublicKey(secret, RoleForward)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("forward identity was not deterministic")
	}
	different, err := IdentitiesDiffer(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !different {
		t.Fatal("forward and exit identities must differ")
	}
}
