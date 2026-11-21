package manifest_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"depotdownloader-go/pkg/manifest"
)

func TestParseRealManifest(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "..", "test_zip_contents", "813231_8951799273146338346.manifest")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Skip("test manifest not found, skipping")
	}

	m, err := manifest.ParseFromFile(manifestPath)
	if err != nil {
		t.Fatalf("ParseFromFile error: %v", err)
	}

	t.Logf("DepotID: %d, GidManifest: %d, Files count: %d, FilenamesEncrypted: %v",
		m.Metadata.DepotID, m.Metadata.GidManifest, len(m.Files), m.FilenamesEncrypted)

	if m.Metadata.DepotID != 813231 {
		t.Errorf("expected depot ID 813231, got %d", m.Metadata.DepotID)
	}

	if m.FilenamesEncrypted {
		// Key from 813230.lua: bb42c41063ff494ceca8a0ce69218fcc97cc5f31c2066992f80ec8dab229486e
		depotKey, err := hex.DecodeString("bb42c41063ff494ceca8a0ce69218fcc97cc5f31c2066992f80ec8dab229486e")
		if err != nil {
			t.Fatalf("hex decode error: %v", err)
		}

		if err := m.DecryptFilenames(depotKey); err != nil {
			t.Fatalf("DecryptFilenames error: %v", err)
		}
	}

	for _, f := range m.Files {
		t.Logf("File: %s, Size: %d, Chunks: %d", f.Filename, f.Size, len(f.Chunks))
	}
}
