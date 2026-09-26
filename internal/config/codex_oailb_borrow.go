package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CodexOaiLBBorrowConfig identifies one donor credential. The management key is
// encrypted at rest with a persistent, instance-local AES-256-GCM data key.
type CodexOaiLBBorrowConfig struct {
	SourceInstanceID    string `yaml:"source-instance-id,omitempty" json:"source-instance-id,omitempty"`
	SourceURL           string `yaml:"source-url" json:"source-url"`
	SourceManagementKey string `yaml:"source-management-key" json:"source-management-key"`
	SourceAuthID        string `yaml:"source-auth-id" json:"source-auth-id"`
	SourceAuthFile      string `yaml:"source-auth-file,omitempty" json:"source-auth-file,omitempty"`
}

func (c *CodexOaiLBBorrowConfig) Validate() error {
	if c == nil {
		return nil
	}
	u, err := url.Parse(c.SourceURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("oailb source-url must be an HTTP(S) instance URL without credentials, query or fragment")
	}
	if strings.TrimSpace(c.SourceAuthID) == "" || strings.TrimSpace(c.SourceManagementKey) == "" {
		return errors.New("oailb source-auth-id and source-management-key are required")
	}
	return nil
}

var oaiLBKeyMu sync.Mutex

func oaiLBDataKey(authDir string, create bool) ([]byte, error) {
	if authDir == "" {
		authDir = DefaultAuthDir
	}
	if authDir == "~" || strings.HasPrefix(authDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if authDir == "~" {
			authDir = home
		} else {
			authDir = filepath.Join(home, strings.TrimPrefix(authDir, "~/"))
		}
	}
	oaiLBKeyMu.Lock()
	defer oaiLBKeyMu.Unlock()
	path := filepath.Join(authDir, ".oailb-data-key")
	raw, err := os.ReadFile(path)
	if err == nil {
		key, errDecode := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if errDecode != nil || len(key) != 32 {
			return nil, errors.New("invalid oailb data key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) || !create {
		return nil, errors.New("oailb data key unavailable")
	}
	if err = os.MkdirAll(authDir, 0700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, errWrite := f.WriteString(base64.RawStdEncoding.EncodeToString(key) + "\n")
	errClose := f.Close()
	if errWrite != nil {
		return nil, errWrite
	}
	if errClose != nil {
		return nil, errClose
	}
	return key, nil
}

func oaiLBAEAD(authDir string, create bool) (cipher.AEAD, error) {
	key, err := oaiLBDataKey(authDir, create)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ProtectCodexOaiLBSecret encrypts plaintext once and preserves ciphertext on
// normal configuration round trips. Donor keys cannot be stored as bcrypt hashes.
func ProtectCodexOaiLBSecret(cfg *Config) error {
	if cfg == nil || cfg.CodexHeaderDefaults.OaiLBBorrow == nil {
		return nil
	}
	c := cfg.CodexHeaderDefaults.OaiLBBorrow
	if err := c.Validate(); err != nil {
		return err
	}
	if strings.HasPrefix(c.SourceManagementKey, "enc:v1:") {
		return nil
	}
	aead, err := oaiLBAEAD(cfg.AuthDir, true)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nil, nonce, []byte(c.SourceManagementKey), []byte("cpa-oailb-borrow-v1"))
	c.SourceManagementKey = "enc:v1:" + base64.RawStdEncoding.EncodeToString(nonce) + ":" + base64.RawStdEncoding.EncodeToString(sealed)
	return nil
}

func CodexOaiLBManagementKey(cfg *Config) (string, error) {
	if cfg == nil || cfg.CodexHeaderDefaults.OaiLBBorrow == nil {
		return "", errors.New("oailb borrowing is disabled")
	}
	value := cfg.CodexHeaderDefaults.OaiLBBorrow.SourceManagementKey
	if !strings.HasPrefix(value, "enc:v1:") {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, "enc:v1:"), ":")
	if len(parts) != 2 {
		return "", errors.New("invalid oailb encrypted key")
	}
	aead, err := oaiLBAEAD(cfg.AuthDir, false)
	if err != nil {
		return "", err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", errors.New("invalid oailb encrypted key nonce")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid oailb encrypted key")
	}
	plain, err := aead.Open(nil, nonce, sealed, []byte("cpa-oailb-borrow-v1"))
	if err != nil {
		return "", errors.New("cannot decrypt oailb management key")
	}
	return string(plain), nil
}
