// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxTokenBytes = 16384

// Tokens encrypts purpose-bound opaque tokens using a configured shared key.
// Tokens protect contents and request bindings. They do not authenticate or authorize callers.
type Tokens struct{ aead cipher.AEAD }

// NewTokens creates encrypted ETag and page tokens from a shared 256-bit key.
// Server replicas must use the same key to accept each other's tokens.
func NewTokens(key []byte) (*Tokens, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("token key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating token cipher: %w", err)
	}
	// This AEAD generates and prepends its nonce, so seal and open intentionally pass a nil nonce.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("creating token envelope: %w", err)
	}
	return &Tokens{aead: aead}, nil
}

// tokenBinding is authenticated alongside the ciphertext to prevent reuse in a different request scope.
type tokenBinding struct {
	Surface  string `json:"surface"`
	Store    string `json:"store"`
	Scope    string `json:"scope"`
	Resource string `json:"resource"`
	Name     string `json:"name,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

// tokenValue stores either a UID/generation precondition or an expiring datastore continuation.
type tokenValue struct {
	UID        string `json:"uid,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Continue   string `json:"continue,omitempty"`
	Expires    int64  `json:"expires,omitempty"`
}

// seal encrypts a token and binds it to its purpose and request scope to prevent reuse elsewhere.
func (t *Tokens) seal(purpose string, binding tokenBinding, value tokenValue) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding token: %w", err)
	}
	aad, _ := json.Marshal(binding)
	sealed := t.aead.Seal(nil, nil, data, append([]byte(purpose+":"), aad...))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open authenticates and decrypts a token, rejecting a mismatched request scope or expired page.
func (t *Tokens) open(purpose string, binding tokenBinding, token string) (tokenValue, error) {
	var value tokenValue
	invalid := Invalid(FieldError{Code: "invalid_token", Message: "The token is invalid for this request."})
	if len(token) == 0 || len(token) > maxTokenBytes || strings.ContainsAny(token, "\" ,*") {
		return value, invalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return value, invalid
	}
	aad, _ := json.Marshal(binding)
	data, err := t.aead.Open(nil, nil, sealed, append([]byte(purpose+":"), aad...))
	if err != nil || json.Unmarshal(data, &value) != nil {
		return value, invalid
	}
	if value.Expires != 0 && time.Now().Unix() >= value.Expires {
		return value, problem(410, "page_expired", "The page has expired.")
	}
	return value, nil
}
