package settings

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// revert-check RC-kdf1: removing the random salt from HashPassword (or
// dropping VerifyPassword entirely) makes this red — the two stored values
// would be identical or would no longer verify.
func TestHashPasswordSaltedFormat(t *testing.T) {
	t.Parallel()
	a := HashPassword("secret")
	b := HashPassword("secret")
	require.NotEqual(t, a, b, "two hashes of the same password must differ (random salt)")
	for _, stored := range []string{a, b} {
		require.True(t, strings.HasPrefix(stored, "pbkdf2-sha256"), "unexpected prefix: %q", stored)
		parts := strings.Split(stored, "$")
		require.Len(t, parts, 4)
		iterations, err := strconv.Atoi(parts[1])
		require.NoError(t, err)
		require.Equal(t, int(settingsPasswordIterations.Load()), iterations)
		salt, err := base64.RawStdEncoding.DecodeString(parts[2])
		require.NoError(t, err)
		require.Len(t, salt, 16)
		key, err := base64.RawStdEncoding.DecodeString(parts[3])
		require.NoError(t, err)
		require.Len(t, key, 32)
		require.True(t, VerifyPassword("secret", stored))
	}
}

// revert-check RC-kdf2: making VerifyPassword accept any password (or compare
// with a wrong-constant-time != ) makes the wrong-password cases pass and
// this test red.
func TestVerifyPasswordWrongPasswordFails(t *testing.T) {
	t.Parallel()
	stored := HashPassword("secret")
	require.False(t, VerifyPassword("wrong", stored))
	require.False(t, VerifyPassword("", stored), "an empty password must not verify against a non-empty password's hash")
}
