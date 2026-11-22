package credentials

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestKeySetEncryptDecrypt(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	keys := &KeySet{activeVersion: 3, keys: map[int][]byte{3: key}}
	ciphertext, version, err := keys.Encrypt([]byte("provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("version = %d, want 3", version)
	}
	plaintext, err := keys.Decrypt(version, ciphertext)
	if err != nil || string(plaintext) != "provider-key" {
		t.Fatalf("Decrypt() = %q, %v", plaintext, err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := keys.Decrypt(version, ciphertext); err == nil {
		t.Fatal("Decrypt() accepted modified ciphertext")
	}
}

func TestLoadKeySet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	data := `{"7":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x07}, 32)) + `"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeySet(path, 7)
	if err != nil || keys.activeVersion != 7 {
		t.Fatalf("LoadKeySet() = %#v, %v", keys, err)
	}
	if _, err := LoadKeySet(path, 8); err == nil {
		t.Fatal("LoadKeySet() accepted a missing active key version")
	}
}
