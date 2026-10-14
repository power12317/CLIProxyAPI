package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOaiLBConfigEncryptedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	input := "# retained\nauth-dir: " + dir + "\ncodex-header-defaults:\n  user-agent: custom\n  oailb-borrow:\n    source-url: https://donor.example/prefix\n    source-auth-id: credential\n    source-management-key: donor-secret\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "donor-secret") || !strings.Contains(string(raw), "enc:v1:") || !strings.Contains(string(raw), "# retained") {
		t.Fatalf("plaintext or lost comment: %s", raw)
	}
	key, err := CodexOaiLBManagementKey(cfg)
	if err != nil || key != "donor-secret" {
		t.Fatal("key did not decrypt", err)
	}
	ciphertext := cfg.CodexHeaderDefaults.OaiLBBorrow.SourceManagementKey
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CodexHeaderDefaults.OaiLBBorrow.SourceManagementKey != ciphertext {
		t.Fatal("ciphertext changed on round trip")
	}
	info, err := os.Stat(filepath.Join(dir, ".oailb-data-key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("data key permissions", info.Mode())
	}
	other := loaded.CloneForRuntime()
	other.AuthDir = t.TempDir()
	if _, err = CodexOaiLBManagementKey(other); err == nil {
		t.Fatal("another instance decrypted the key")
	}
	if _, err = os.Stat(filepath.Join(other.AuthDir, ".oailb-data-key")); !os.IsNotExist(err) {
		t.Fatal("decrypt generated a replacement key")
	}
	loaded.CodexHeaderDefaults.OaiLBBorrow = nil
	if err = SaveConfigPreserveComments(path, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CodexHeaderDefaults.OaiLBBorrow != nil || loaded.CodexHeaderDefaults.UserAgent != "custom" {
		t.Fatal("disable did not remove borrowing or changed other headers")
	}
}

func TestOaiLBRejectsInvalidSourceConfig(t *testing.T) {
	for _, source := range []string{"file:///tmp/donor", "https://user:pass@example.com", "https://example.com?q=x", ""} {
		c := &CodexOaiLBBorrowConfig{SourceURL: source, SourceAuthID: "a", SourceManagementKey: "k"}
		if c.Validate() == nil {
			t.Fatalf("accepted %q", source)
		}
	}
}
