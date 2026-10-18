// Package credentials verifies local prototype passwords without retaining their
// plaintext in the verifier. Persistent issuance and account administration are
// separate from this immutable, concurrently readable verifier.
package credentials

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

type (
	hash     struct{ salt, key []byte }
	Verifier struct {
		users map[string]hash
		dummy hash
	}
)

// HashPassword returns a versioned, salted encoding suitable for persistence.
// The fixed cost prevents an untrusted encoding from requesting arbitrary work.
func HashPassword(password string) (string, error) {
	if password == "" || len(password) > 4096 {
		return "", fmt.Errorf("credentials: invalid password")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$600000$" + base64.RawStdEncoding.EncodeToString(
		salt,
	) + "$" + base64.RawStdEncoding.EncodeToString(
		key,
	), nil
}

// VerifyHash also performs one derivation for missing or malformed records.
func VerifyHash(encoded, password string) bool {
	if len(password) > 4096 {
		return false
	}
	salt, want, valid := decodeHash(encoded)
	key, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return err == nil && subtle.ConstantTimeCompare(key, want) == 1 && valid
}

// decodeHash supplies fixed-size dummy inputs for malformed records so callers
// perform the same derivation regardless of whether an account exists.
func decodeHash(encoded string) ([]byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	salt, want := make([]byte, 32), make([]byte, 32)
	valid := false
	if len(parts) == 4 && parts[0] == "pbkdf2-sha256" && parts[1] == "600000" {
		s, e1 := base64.RawStdEncoding.DecodeString(parts[2])
		k, e2 := base64.RawStdEncoding.DecodeString(parts[3])
		if e1 == nil && e2 == nil && len(s) == 32 && len(k) == 32 {
			salt, want, valid = s, k, true
		}
	}
	return salt, want, valid
}

func New(passwords map[string]string) (*Verifier, error) {
	if len(passwords) == 0 {
		return nil, fmt.Errorf("credentials: accounts required")
	}
	v := &Verifier{users: map[string]hash{}}
	for user, password := range passwords {
		if user == "" || password == "" || len(user) > 1024 || len(password) > 4096 {
			return nil, fmt.Errorf("credentials: invalid account")
		}
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		key, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
		if err != nil {
			return nil, err
		}
		h := hash{salt, key}
		v.users[user] = h
		v.dummy = h
	}
	return v, nil
}

func (v *Verifier) Verify(user, password string) bool {
	if len(password) > 4096 {
		return false
	}
	h, exists := v.users[user]
	if !exists {
		h = v.dummy
	}
	key, err := pbkdf2.Key(sha256.New, password, h.salt, 600000, 32)
	return err == nil && subtle.ConstantTimeCompare(key, h.key) == 1 && exists
}
