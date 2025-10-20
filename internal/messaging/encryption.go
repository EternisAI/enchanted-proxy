package messaging

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"

	"golang.org/x/crypto/hkdf"
)

// EncryptionService handles ECDH + AES-GCM message encryption
type EncryptionService struct{}

// NewEncryptionService creates a new encryption service
func NewEncryptionService() *EncryptionService {
	return &EncryptionService{}
}

// EncryptMessage encrypts message content using ECDH + HKDF + AES-256-GCM
// Returns base64-encoded: ephemeralPublicKey || nonce || ciphertext || tag
func (e *EncryptionService) EncryptMessage(content string, publicKeyJWK string) (string, error) {
	// Parse JWK public key
	recipientPubKey, err := e.parseJWKPublicKey(publicKeyJWK)
	if err != nil {
		return "", fmt.Errorf("failed to parse JWK public key: %w", err)
	}

	// Generate ephemeral ECDH key pair
	curve := ecdh.P256()
	ephemeralPrivKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("failed to generate ephemeral key: %w", err)
	}

	// Convert recipient's ECDSA public key to ECDH public key
	recipientECDHPubKey, err := curve.NewPublicKey(elliptic.Marshal(elliptic.P256(), recipientPubKey.X, recipientPubKey.Y))
	if err != nil {
		return "", fmt.Errorf("failed to convert public key to ECDH: %w", err)
	}

	// Perform ECDH key agreement
	sharedSecret, err := ephemeralPrivKey.ECDH(recipientECDHPubKey)
	if err != nil {
		return "", fmt.Errorf("ECDH key agreement failed: %w", err)
	}

	// Derive AES key using HKDF
	aesKey := make([]byte, 32) // AES-256
	kdf := hkdf.New(sha256.New, sharedSecret, nil, []byte("message-encryption"))
	if _, err := io.ReadFull(kdf, aesKey); err != nil {
		return "", fmt.Errorf("key derivation failed: %w", err)
	}

	// Create AES-GCM cipher
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt plaintext
	ciphertext := gcm.Seal(nil, nonce, []byte(content), nil)

	// Encode as: ephemeralPublicKey || nonce || ciphertext (includes auth tag)
	ephemeralPubKeyBytes := ephemeralPrivKey.PublicKey().Bytes()
	result := make([]byte, 0, len(ephemeralPubKeyBytes)+len(nonce)+len(ciphertext))
	result = append(result, ephemeralPubKeyBytes...)
	result = append(result, nonce...)
	result = append(result, ciphertext...)

	return base64.StdEncoding.EncodeToString(result), nil
}

// parseJWKPublicKey parses a JWK JSON string to an ECDSA public key
func (e *EncryptionService) parseJWKPublicKey(jwkJSON string) (*ecdsa.PublicKey, error) {
	var jwk JWKPublicKey
	if err := json.Unmarshal([]byte(jwkJSON), &jwk); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JWK: %w", err)
	}

	// Validate key type
	if jwk.Kty != "EC" {
		return nil, fmt.Errorf("invalid key type: expected EC, got %s", jwk.Kty)
	}
	if jwk.Crv != "P-256" {
		return nil, fmt.Errorf("invalid curve: expected P-256, got %s", jwk.Crv)
	}

	// Decode base64url-encoded coordinates
	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("failed to decode X coordinate: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("failed to decode Y coordinate: %w", err)
	}

	// Create public key
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)

	pubKey := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     x,
		Y:     y,
	}

	// Validate the public key is on the curve
	if !elliptic.P256().IsOnCurve(x, y) {
		return nil, fmt.Errorf("public key is not on P-256 curve")
	}

	return pubKey, nil
}

// ValidatePublicKey validates a JWK public key
func (e *EncryptionService) ValidatePublicKey(publicKeyJWK string) error {
	_, err := e.parseJWKPublicKey(publicKeyJWK)
	return err
}
