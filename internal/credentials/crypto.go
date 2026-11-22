package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
)

// KeySet encrypts provider credentials before they enter durable storage.
type KeySet struct {
	activeVersion int
	keys          map[int][]byte
}

// LoadKeySet reads a JSON object mapping positive key versions to base64-encoded 32-byte keys.
func LoadKeySet(path string, activeVersion int) (*KeySet, error) {
	if path == "" || activeVersion < 1 {
		return nil, fmt.Errorf("encryption key file and positive active key version are required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential encryption keys: %w", err)
	}
	var encoded map[string]string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return nil, fmt.Errorf("parse credential encryption keys: %w", err)
	}
	keys := make(map[int][]byte, len(encoded))
	for versionText, value := range encoded {
		version, err := strconv.Atoi(versionText)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("invalid credential encryption key version")
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("credential encryption key version %d must be a base64-encoded 32-byte key", version)
		}
		keys[version] = key
	}
	if _, ok := keys[activeVersion]; !ok {
		return nil, fmt.Errorf("active credential encryption key version %d is unavailable", activeVersion)
	}
	return &KeySet{activeVersion: activeVersion, keys: keys}, nil
}

// Encrypt returns authenticated ciphertext and the key version used.
func (k *KeySet) Encrypt(plaintext []byte) ([]byte, int, error) {
	key := k.keys[k.activeVersion]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, 0, fmt.Errorf("initialize credential encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, fmt.Errorf("initialize credential encryption: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext := aead.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, k.activeVersion, nil
}

// Decrypt authenticates and returns ciphertext using the recorded key version.
func (k *KeySet) Decrypt(version int, ciphertext []byte) ([]byte, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("credential encryption key version %d is unavailable", version)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize credential decryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential decryption: %w", err)
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("invalid encrypted credential")
	}
	nonce, payload := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider credential: %w", err)
	}
	return plaintext, nil
}
