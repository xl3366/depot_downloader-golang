package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

var (
	ErrInvalidKeySize    = errors.New("invalid depot key size (must be 32 bytes)")
	ErrDataTooShort      = errors.New("encrypted data too short")
	ErrInvalidPadding    = errors.New("invalid PKCS7 padding")
	ErrInvalidBlockSize  = errors.New("data length is not a multiple of AES block size")
)

// PKCS7Unpad removes PKCS#7 padding from plaintext.
func PKCS7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, ErrInvalidPadding
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > len(data) || padLen > aes.BlockSize {
		return nil, ErrInvalidPadding
	}
	for i := len(data) - padLen; i < len(data); i++ {
		if data[i] != byte(padLen) {
			return nil, ErrInvalidPadding
		}
	}
	return data[:len(data)-padLen], nil
}

// DecryptECBBlock decrypts a single 16-byte block using AES-256-ECB.
func DecryptECBBlock(key []byte, src []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKeySize
	}
	if len(src) < aes.BlockSize {
		return nil, ErrDataTooShort
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	dst := make([]byte, aes.BlockSize)
	block.Decrypt(dst, src[:aes.BlockSize])
	return dst, nil
}

// DecryptChunk decrypts an encrypted Steam chunk with AES-256-ECB(IV) + AES-256-CBC(Body).
func DecryptChunk(key []byte, encrypted []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKeySize
	}
	if len(encrypted) < 32 || (len(encrypted)-aes.BlockSize)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: length %d", ErrInvalidBlockSize, len(encrypted))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// 1. Decrypt first 16 bytes using ECB to get CBC IV
	iv := make([]byte, aes.BlockSize)
	block.Decrypt(iv, encrypted[:aes.BlockSize])

	// 2. Decrypt remaining bytes using CBC mode
	ciphertext := encrypted[aes.BlockSize:]
	mode := cipher.NewCBCDecrypter(block, iv)

	plain := make([]byte, len(ciphertext))
	mode.CryptBlocks(plain, ciphertext)

	// 3. Remove PKCS7 padding
	unpadded, err := PKCS7Unpad(plain)
	if err != nil {
		return nil, fmt.Errorf("chunk padding error: %w", err)
	}

	return unpadded, nil
}

// DecryptFilename decrypts a base64-encoded encrypted filename from the manifest.
func DecryptFilename(key []byte, encoded string) (string, error) {
	// Remove any ascii whitespace (Steam line wraps long filenames)
	var sb strings.Builder
	for _, r := range encoded {
		if !unicode.IsSpace(r) {
			sb.WriteRune(r)
		}
	}
	cleaned := sb.String()

	blob, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", fmt.Errorf("invalid base64 filename: %w", err)
	}

	if len(blob) < 32 || len(blob)%aes.BlockSize != 0 {
		return "", fmt.Errorf("invalid encrypted filename length %d", len(blob))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	// Decrypt IV with ECB
	iv := make([]byte, aes.BlockSize)
	block.Decrypt(iv, blob[:aes.BlockSize])

	// Decrypt body with CBC
	ciphertext := blob[aes.BlockSize:]
	mode := cipher.NewCBCDecrypter(block, iv)
	plain := make([]byte, len(ciphertext))
	mode.CryptBlocks(plain, ciphertext)

	unpadded, err := PKCS7Unpad(plain)
	if err != nil {
		return "", fmt.Errorf("filename padding error: %w", err)
	}

	// Steam NUL-terminates the string
	for len(unpadded) > 0 && unpadded[len(unpadded)-1] == 0 {
		unpadded = unpadded[:len(unpadded)-1]
	}

	// Canonicalize path separators to slash '/' and clean leading slashes/control characters
	str := strings.ReplaceAll(string(unpadded), "\\", "/")
	str = strings.TrimLeft(str, "/")
	var cleanedStr strings.Builder
	for _, r := range str {
		if !unicode.IsControl(r) {
			cleanedStr.WriteRune(r)
		}
	}
	return cleanedStr.String(), nil
}
