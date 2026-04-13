// Keep in sync with internal/crypto/aes.go — both files are intentionally
// identical (separate Go modules cannot share packages directly).

// Package crypto provides streaming AES-256-GCM encryption/decryption for S3
// objects. Files are split into 64 KB plaintext chunks; each chunk is
// independently sealed with AES-GCM so the stream can be decrypted without
// buffering the entire object.
//
// Wire format:
//
//	[8-byte random nonce prefix]
//	[N × (plaintext chunk sealed with AES-256-GCM)]
//
// The 12-byte GCM nonce for chunk i is built as:
//
//	nonce_prefix (8 bytes) || big_endian_uint32(i) (4 bytes)
//
// Each ciphertext chunk is chunkPlainSize+gcmTagSize bytes, except the last
// which may be shorter. An empty plaintext produces exactly one empty chunk
// (16-byte GCM tag only).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	chunkPlainSize  = 64 * 1024                       // 64 KB plaintext per chunk
	gcmTagSize      = 16                               // AES-GCM authentication tag
	chunkCipherSize = chunkPlainSize + gcmTagSize      // 65552 bytes ciphertext per full chunk
	noncePrefixLen  = 8                                // random per-file bytes
	gcmNonceLen     = 12                               // AES-GCM requires 12-byte nonce
)

// ParseKey decodes a hex-encoded 32-byte AES-256 key.
// Returns nil (encryption disabled) when keyHex is empty.
func ParseKey(keyHex string) ([]byte, error) {
	if keyHex == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("encryption key: invalid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key: must be 32 bytes (64 hex chars), got %d bytes", len(key))
	}
	return key, nil
}

// EncryptedSize returns the byte length of the encrypted output for a given
// plaintext size. Returns -1 when plainSize is negative (unknown).
func EncryptedSize(plainSize int64) int64 {
	if plainSize < 0 {
		return -1
	}
	// Full chunks + last (possibly empty) partial chunk.
	fullChunks := plainSize / chunkPlainSize
	lastPlain := plainSize % chunkPlainSize
	size := int64(noncePrefixLen) + fullChunks*int64(chunkCipherSize)
	// A final partial chunk exists only when the input is empty (one 0-byte
	// chunk, sealed to a 16-byte GCM tag) or when the input is not chunk-aligned.
	// Exactly-aligned non-zero inputs produce only full chunks — no extra chunk.
	if plainSize == 0 || lastPlain > 0 {
		size += lastPlain + int64(gcmTagSize)
	}
	return size
}

// Encrypt returns a reader that streams the AES-256-GCM encryption of src.
// If key is nil the original src is returned unchanged.
func Encrypt(key []byte, src io.Reader) io.ReadCloser {
	if key == nil {
		return io.NopCloser(src)
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(encryptStream(key, src, pw))
	}()
	return pr
}

// Decrypt returns a reader that streams the AES-256-GCM decryption of src.
// If key is nil the original src is returned unchanged.
func Decrypt(key []byte, src io.ReadCloser) io.ReadCloser {
	if key == nil {
		return src
	}
	pr, pw := io.Pipe()
	go func() {
		err := decryptStream(key, src, pw)
		src.Close()
		pw.CloseWithError(err)
	}()
	return pr
}

// encryptStream writes the encrypted stream to dst.
func encryptStream(key []byte, src io.Reader, dst io.Writer) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	noncePrefix := make([]byte, noncePrefixLen)
	if _, err := rand.Read(noncePrefix); err != nil {
		return fmt.Errorf("generating nonce: %w", err)
	}
	if _, err := dst.Write(noncePrefix); err != nil {
		return err
	}

	buf := make([]byte, chunkPlainSize)
	nonce := make([]byte, gcmNonceLen)
	copy(nonce, noncePrefix)

	for i := uint32(0); ; i++ {
		binary.BigEndian.PutUint32(nonce[noncePrefixLen:], i)

		n, readErr := io.ReadFull(src, buf)

		// Always seal at least once (i == 0) to handle empty inputs.
		if n > 0 || i == 0 {
			sealed := gcm.Seal(nil, nonce, buf[:n], nil)
			if _, err := dst.Write(sealed); err != nil {
				return err
			}
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// decryptStream reads the encrypted stream from src and writes plaintext to dst.
func decryptStream(key []byte, src io.Reader, dst io.Writer) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	noncePrefix := make([]byte, noncePrefixLen)
	if _, err := io.ReadFull(src, noncePrefix); err != nil {
		return fmt.Errorf("reading nonce prefix: %w", err)
	}

	buf := make([]byte, chunkCipherSize)
	nonce := make([]byte, gcmNonceLen)
	copy(nonce, noncePrefix)

	for i := uint32(0); ; i++ {
		binary.BigEndian.PutUint32(nonce[noncePrefixLen:], i)

		n, readErr := io.ReadFull(src, buf)
		if n == 0 {
			if readErr == io.EOF {
				return nil // clean end of stream
			}
			return readErr
		}

		plain, err := gcm.Open(nil, nonce, buf[:n], nil)
		if err != nil {
			return fmt.Errorf("decrypting chunk %d: %w", i, err)
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
