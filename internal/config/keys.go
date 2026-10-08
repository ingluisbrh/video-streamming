// Package config loads the single master key (32 bytes, AES-256).
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const keyFilePerm = 0600

type keyJSON struct {
	MasterKeyHex string `json:"masterKeyHex"`
}

// DefaultPaths searched when no explicit key file is given and env is empty.
var DefaultPaths = []string{"keys.json", "config/keys.json"}

// LoadKey returns the 32-byte master key.
// Precedence: explicit path flag > STREAM_KEY_HEX env > DefaultPaths.
func LoadKey(explicitPath string) ([]byte, string, error) {
	if explicitPath != "" {
		k, err := loadFile(explicitPath)
		return k, explicitPath, err
	}
	if env := strings.TrimSpace(os.Getenv("STREAM_KEY_HEX")); env != "" {
		k, err := hex.DecodeString(env)
		if err != nil {
			return nil, "env:STREAM_KEY_HEX", fmt.Errorf("bad hex in STREAM_KEY_HEX: %w", err)
		}
		if len(k) != 32 {
			return nil, "env:STREAM_KEY_HEX", errors.New("STREAM_KEY_HEX must decode to 32 bytes")
		}
		return k, "env:STREAM_KEY_HEX", nil
	}
	for _, p := range DefaultPaths {
		if _, err := os.Stat(p); err == nil {
			k, err := loadFile(p)
			return k, p, err
		}
	}
	return nil, "", errors.New("no key found: set STREAM_KEY_HEX or create keys.json (see scripts/keygen.sh)")
}

func loadFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kj keyJSON
	// Accept bare hex string too, for convenience.
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &kj); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else {
		kj.MasterKeyHex = trimmed
	}
	k, err := hex.DecodeString(strings.TrimSpace(kj.MasterKeyHex))
	if err != nil {
		return nil, fmt.Errorf("bad hex in %s: %w", path, err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("key in %s must be 32 bytes, got %d", path, len(k))
	}
	return k, nil
}

// GenerateKeyFile creates a new random master key at path (refuses to overwrite).
func GenerateKeyFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists, refusing to overwrite", path)
	}
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	raw, _ := json.MarshalIndent(keyJSON{MasterKeyHex: hex.EncodeToString(k[:])}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), keyFilePerm); err != nil {
		return err
	}
	return nil
}
