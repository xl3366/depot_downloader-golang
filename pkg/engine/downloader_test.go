package engine_test

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"depotdownloader-go/pkg/crypto"
	"depotdownloader-go/pkg/manifest"
	"depotdownloader-go/pkg/storage"
)

func TestBreakpointResumeLogic(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "depot_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	testFile := filepath.Join(tempDir, "test.bin")
	chunk1Data := []byte("Chunk 1 test data 1234567890abcdef")
	chunk2Data := []byte("Chunk 2 test data 0987654321fedcba")

	// Pre-create the file on disk with chunk 1 valid, but chunk 2 missing/empty
	f, err := os.Create(testFile)
	if err != nil {
		t.Fatal(err)
	}
	// Write chunk 1
	_, _ = f.WriteAt(chunk1Data, 0)
	// Truncate to total size of chunk 1 + chunk 2
	totalSize := int64(len(chunk1Data) + len(chunk2Data))
	_ = f.Truncate(totalSize)
	_ = f.Close()

	// Verify chunk 1 Adler32 matches
	crc1 := crypto.Adler32Seed0(chunk1Data)
	crc2 := crypto.Adler32Seed0(chunk2Data)

	// Read file back and check chunk 1 vs chunk 2
	rf, err := os.Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	buf1 := make([]byte, len(chunk1Data))
	_, _ = rf.ReadAt(buf1, 0)
	if crypto.Adler32Seed0(buf1) != crc1 {
		t.Errorf("chunk 1 crc expected 0x%08X, got 0x%08X", crc1, crypto.Adler32Seed0(buf1))
	}

	buf2 := make([]byte, len(chunk2Data))
	_, _ = rf.ReadAt(buf2, int64(len(chunk1Data)))
	if crypto.Adler32Seed0(buf2) == crc2 {
		t.Errorf("chunk 2 should be empty/mismatch, but matched")
	}
}

func TestIncrementalUpdateManifest(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "depot_inc_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	store := storage.LoadState(tempDir)

	// 1. Commit dummy old manifest
	depotID := uint32(1001)
	oldGid := uint64(111111)
	err = store.CommitDepot(depotID, oldGid, []byte("dummy manifest"))
	if err != nil {
		t.Fatal(err)
	}

	installedGid, ok := store.GetInstalledGid(depotID)
	if !ok || installedGid != oldGid {
		t.Fatalf("expected installed gid %d, got %d", oldGid, installedGid)
	}

	// 2. Commit new manifest gid
	newGid := uint64(222222)
	err = store.CommitDepot(depotID, newGid, []byte("new dummy manifest"))
	if err != nil {
		t.Fatal(err)
	}

	installedGid, ok = store.GetInstalledGid(depotID)
	if !ok || installedGid != newGid {
		t.Fatalf("expected updated installed gid %d, got %d", newGid, installedGid)
	}
}

func sha1Bytes(data []byte) []byte {
	h := sha1.New()
	h.Write(data)
	return h.Sum(nil)
}

func makeDummyChunk(data []byte, offset uint64) *manifest.ChunkData {
	return &manifest.ChunkData{
		Sha:        sha1Bytes(data),
		Crc:        crypto.Adler32Seed0(data),
		Offset:     offset,
		CbOriginal: uint32(len(data)),
	}
}
