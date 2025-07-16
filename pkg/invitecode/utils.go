package invitecode

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
)

// GenerateNanoID creates a new nanoid with custom alphabet (no confusing characters).
func GenerateNanoID() (string, error) {
	return GenerateNanoIDWithLength(10)
}

// GenerateNanoIDWithLength creates a new nanoid with specified length.
func GenerateNanoIDWithLength(length int) (string, error) {
	// Custom alphabet excluding 0/O/1/I for clarity
	alphabet := "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	for i, b := range bytes {
		bytes[i] = alphabet[b%byte(len(alphabet))]
	}

	return string(bytes), nil
}

// GenerateCodeWithPrefix creates a code with a prefix followed by random characters.
func GenerateCodeWithPrefix(prefix string, totalLength int) (string, error) {
	if len(prefix) >= totalLength {
		return prefix, nil
	}

	remainingLength := totalLength - len(prefix)
	suffix, err := GenerateNanoIDWithLength(remainingLength)
	if err != nil {
		return "", err
	}

	return prefix + suffix, nil
}

// HashCode creates SHA256 hash of the invite code.
func HashCode(code string) string {
	hash := sha256.Sum256([]byte(code))
	return fmt.Sprintf("%x", hash)
}

// SetCodeAndHash generates a new code and sets both code and hash.
func SetCodeAndHash() (string, string, error) {
	code, err := GenerateNanoID()
	if err != nil {
		return "", "", err
	}
	codeHash := HashCode(code)
	return code, codeHash, nil
}

// IsExpired checks if the invite code has expired.
func IsExpired(ic *pgdb.InviteCode) bool {
	if ic.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*ic.ExpiresAt)
}

// CanBeUsed checks if the invite code can still be used.
func CanBeUsed(ic *pgdb.InviteCode) bool {
	return ic.IsActive && !IsExpired(ic) && !ic.IsUsed
}
