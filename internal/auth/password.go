package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	// CurrentPasswordParamsVersion records the OWASP Argon2id minimum profile
	// observed on 2026-08-29: 19 MiB, two iterations, one lane.
	CurrentPasswordParamsVersion = "argon2id-v1-m19456-t2-p1"
	minimumPasswordBytes         = 12
	maximumPasswordBytes         = 1024
)

type passwordParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	saltLength  uint32
	keyLength   uint32
}

var currentPasswordParams = passwordParams{
	memory:      19 * 1024,
	iterations:  2,
	parallelism: 1,
	saltLength:  16,
	keyLength:   32,
}

// HashPassword returns a PHC-formatted Argon2id hash and its parameter version.
func HashPassword(password string) (string, string, error) {
	return hashPassword(password, rand.Reader, currentPasswordParams)
}

func hashPassword(password string, random io.Reader, params passwordParams) (string, string, error) {
	if err := validatePassword(password); err != nil {
		return "", "", err
	}
	salt := make([]byte, params.saltLength)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", "", fmt.Errorf("generate password salt: %w", err)
	}
	derived := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, params.keyLength)
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.memory,
		params.iterations,
		params.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	)
	return encoded, CurrentPasswordParamsVersion, nil
}

// VerifyPassword checks a PHC Argon2id hash in constant time.
func VerifyPassword(password, encoded string) (bool, error) {
	if len(password) > maximumPasswordBytes {
		return false, ErrInvalidCredentials
	}
	params, salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// PasswordNeedsRehash reports whether a valid stored hash uses older parameters.
func PasswordNeedsRehash(encoded, paramsVersion string) bool {
	params, salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return true
	}
	return paramsVersion != CurrentPasswordParamsVersion ||
		params.memory != currentPasswordParams.memory ||
		params.iterations != currentPasswordParams.iterations ||
		params.parallelism != currentPasswordParams.parallelism ||
		uint32(len(salt)) != currentPasswordParams.saltLength ||
		uint32(len(expected)) != currentPasswordParams.keyLength
}

func validatePassword(password string) error {
	length := len([]byte(password))
	if length < minimumPasswordBytes || length > maximumPasswordBytes {
		return ErrInvalidInput
	}
	return nil
}

func parsePasswordHash(encoded string) (passwordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return passwordParams{}, nil, nil, errors.New("invalid Argon2id hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version || parts[2] != fmt.Sprintf("v=%d", version) {
		return passwordParams{}, nil, nil, errors.New("unsupported Argon2id version")
	}
	var params passwordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallelism); err != nil ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", params.memory, params.iterations, params.parallelism) {
		return passwordParams{}, nil, nil, errors.New("invalid Argon2id parameters")
	}
	if params.memory < 7*1024 || params.memory > 256*1024 || params.iterations < 1 || params.iterations > 10 || params.parallelism < 1 || params.parallelism > 8 {
		return passwordParams{}, nil, nil, errors.New("unsafe Argon2id parameters")
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return passwordParams{}, nil, nil, errors.New("invalid Argon2id salt")
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return passwordParams{}, nil, nil, errors.New("invalid Argon2id key")
	}
	params.saltLength = uint32(len(salt))
	params.keyLength = uint32(len(expected))
	return params, salt, expected, nil
}
