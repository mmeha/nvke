// SPDX-License-Identifier: Apache-2.0

package resource

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTokens(t *testing.T) {
	tokens, err := NewTokens(make([]byte, 32))
	require.NoError(t, err)
	binding := tokenBinding{Surface: "one", Store: "test", Scope: "org-a", Resource: "widgets", Name: "example"}
	value := tokenValue{UID: "internal-uid", Generation: 7}
	sealed, err := tokens.seal("version", binding, value)
	require.NoError(t, err)
	require.NotContains(t, sealed, "internal-uid")
	opened, err := tokens.open("version", binding, sealed)
	require.NoError(t, err)
	require.Equal(t, value, opened)
	for name, change := range map[string]func(*tokenBinding){
		"Surface":   func(b *tokenBinding) { b.Surface = "other" },
		"Store":     func(b *tokenBinding) { b.Store = "other" },
		"Namespace": func(b *tokenBinding) { b.Scope = "other" },
		"Resource":  func(b *tokenBinding) { b.Resource = "other" },
		"Name":      func(b *tokenBinding) { b.Name = "other" },
		"PageSize":  func(b *tokenBinding) { b.PageSize = 1 },
	} {
		t.Run("Wrong"+name, func(t *testing.T) {
			other := binding
			change(&other)
			_, err := tokens.open("version", other, sealed)
			require.Error(t, err)
		})
	}
	t.Run("WrongPurpose", func(t *testing.T) {
		_, err := tokens.open("page", binding, sealed)
		require.Error(t, err)
	})
	for name, bad := range map[string]string{
		"Empty": "", "Wildcard": "*", "Multiple": sealed + ",other", "Tampered": sealed[:len(sealed)-3] + "bad",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.open("version", binding, bad)
			require.Error(t, err)
		})
	}
	expired, err := tokens.seal("page", binding,
		tokenValue{Continue: "internal-cursor", Expires: time.Now().Add(-time.Minute).Unix()})
	require.NoError(t, err)
	_, err = tokens.open("page", binding, expired)
	statusCode(t, err, 410)
	for size := range 28 {
		_, err := tokens.open("version", binding, base64.RawURLEncoding.EncodeToString(make([]byte, size)))
		require.Error(t, err)
	}
}
