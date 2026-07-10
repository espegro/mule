package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const MinSecretBytes = 32

var (
	ErrSecretTooShort = errors.New("secret must decode to at least 32 bytes")
	ErrSecretEncoding = errors.New("secret must be base64 or hex encoded")
)

func LoadSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open secret file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat secret file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("secret file must be a regular file")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secret file permissions %04o are too open; use 0600 or stricter", st.Mode().Perm())
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	return DecodeSecret(string(data))
}

func DecodeSecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrSecretTooShort
	}

	if out, err := hex.DecodeString(s); err == nil {
		return validateSecret(out)
	}
	if out, err := base64.StdEncoding.DecodeString(s); err == nil {
		return validateSecret(out)
	}
	if out, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return validateSecret(out)
	}
	return nil, ErrSecretEncoding
}

func validateSecret(secret []byte) ([]byte, error) {
	if len(secret) < MinSecretBytes {
		return nil, ErrSecretTooShort
	}
	out := make([]byte, len(secret))
	copy(out, secret)
	return out, nil
}

func GenerateSecretFile(path string) error {
	secret := make([]byte, MinSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate secret: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(secret) + "\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create secret file: %w", err)
	}
	if _, err := f.WriteString(encoded); err != nil {
		_ = f.Close()
		return fmt.Errorf("write secret file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close secret file: %w", err)
	}
	return nil
}
