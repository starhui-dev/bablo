package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const opaqueTokenBytes = 32

func newOpaqueToken() (string, [32]byte, error) {
	return newOpaqueTokenFrom(rand.Reader)
}

func newOpaqueTokenFrom(random io.Reader) (string, [32]byte, error) {
	var raw [opaqueTokenBytes]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return "", [32]byte{}, fmt.Errorf("generate authentication token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, sha256.Sum256([]byte(token)), nil
}

func hashToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

func newRecoveryCode() (string, [32]byte, error) {
	var raw [10]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", [32]byte{}, fmt.Errorf("generate recovery code: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	code := strings.Join([]string{encoded[0:4], encoded[4:8], encoded[8:12], encoded[12:16]}, "-")
	return code, hashRecoveryCode(code), nil
}

func hashRecoveryCode(code string) [32]byte {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return sha256.Sum256([]byte(normalized))
}
