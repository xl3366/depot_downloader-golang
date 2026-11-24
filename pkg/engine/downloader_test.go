package engine_test

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"depotdownloader-go/pkg/crypto"
	"depotdownloader-go/pkg/engine"
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

func TestAlreadyCommittedSameManifestSkip(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "depot_same_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	data := []byte("Some existing file content on disk")
	filePath := filepath.Join(tempDir, "game.exe")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatal(err)
	}

	depotID := uint32(2001)
	gid := uint64(55555)

	store := storage.LoadState(tempDir)
	_ = store.CommitDepot(depotID, gid, nil)

	m := &manifest.DepotManifest{
		Metadata: &manifest.ContentManifestMetadata{
			DepotID:     depotID,
			GidManifest: gid,
		},
		Files: []*manifest.FileMapping{
			{
				Filename:   "game.exe",
				Size:       uint64(len(data)),
				ShaContent: sha1Bytes(data),
				Chunks:     []*manifest.ChunkData{makeDummyChunk(data, 0)},
			},
		},
	}

	d := engine.NewDownloader(nil, nil, store)

	// In non-verify mode (no -i), it should skip instantly with 0 downloads and no error even with nil server pool
	opts := engine.DownloadOptions{
		OutputDir:       tempDir,
		VerifyIntegrity: false,
	}
	err = d.DownloadManifest(t.Context(), m, make([]byte, 32), opts)
	if err != nil {
		t.Fatalf("expected already committed depot to skip without error, got: %v", err)
	}
}

func sha1Bytes(data []byte) []byte {
	h := sha1.New()
	h.Write(data)
	return h.Sum(nil)
}

func TestMultiDepotCleanupUnmatched(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "depot_cleanup_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Create test files
	depot1File := filepath.Join(tempDir, "depot1_game.exe")
	depot2File := filepath.Join(tempDir, "depot2_content.pak")
	subDir := filepath.Join(tempDir, "dlc")
	_ = os.MkdirAll(subDir, 0755)
	depot2SubFile := filepath.Join(subDir, "dlc1.pak")

	unmatchedFile := filepath.Join(tempDir, "unmatched_old.tmp")
	emptySubDir := filepath.Join(tempDir, "empty_dir")
	_ = os.MkdirAll(emptySubDir, 0755)
	unmatchedInSub := filepath.Join(emptySubDir, "junk.txt")

	stateDir := filepath.Join(tempDir, storage.StateDirName)
	_ = os.MkdirAll(stateDir, 0755)
	stateFile := filepath.Join(stateDir, storage.StateFileName)

	_ = os.WriteFile(depot1File, []byte("depot1"), 0644)
	_ = os.WriteFile(depot2File, []byte("depot2"), 0644)
	_ = os.WriteFile(depot2SubFile, []byte("depot2 dlc"), 0644)
	_ = os.WriteFile(unmatchedFile, []byte("junk"), 0644)
	_ = os.WriteFile(unmatchedInSub, []byte("junk"), 0644)
	_ = os.WriteFile(stateFile, []byte("{}"), 0644)

	allowedFiles := map[string]bool{
		filepath.Clean("depot1_game.exe"):   true,
		filepath.Clean("depot2_content.pak"): true,
		filepath.Clean("dlc/dlc1.pak"):       true,
		filepath.Clean("dlc"):                true,
	}

	count, err := engine.CleanupUnmatched(tempDir, allowedFiles)
	if err != nil {
		t.Fatalf("CleanupUnmatched failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 deleted files, got %d", count)
	}

	// Verify valid files preserved
	if _, err := os.Stat(depot1File); err != nil {
		t.Errorf("depot1File was unexpectedly deleted")
	}
	if _, err := os.Stat(depot2File); err != nil {
		t.Errorf("depot2File was unexpectedly deleted")
	}
	if _, err := os.Stat(depot2SubFile); err != nil {
		t.Errorf("depot2SubFile was unexpectedly deleted")
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("stateFile in .DepotDownloader was unexpectedly deleted")
	}

	// Verify unmatched deleted
	if _, err := os.Stat(unmatchedFile); !os.IsNotExist(err) {
		t.Errorf("unmatchedFile should be deleted")
	}
	if _, err := os.Stat(unmatchedInSub); !os.IsNotExist(err) {
		t.Errorf("unmatchedInSub should be deleted")
	}
	if _, err := os.Stat(emptySubDir); !os.IsNotExist(err) {
		t.Errorf("emptySubDir should have been removed after cleaning up its files")
	}
}

func makeDummyChunk(data []byte, offset uint64) *manifest.ChunkData {
	return &manifest.ChunkData{
		Sha:        sha1Bytes(data),
		Crc:        crypto.Adler32Seed0(data),
		Offset:     offset,
		CbOriginal: uint32(len(data)),
	}
}

func TestVerifyIntegrityDetectsCorruption(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "depot_verify_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	data := []byte("Valid game content for SHA-1 and chunk checking 1234567890")
	filePath := filepath.Join(tempDir, "game.bin")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatal(err)
	}

	chunk := makeDummyChunk(data, 0)
	m := &manifest.DepotManifest{
		Metadata: &manifest.ContentManifestMetadata{
			DepotID:     12345,
			GidManifest: 99999,
		},
		Files: []*manifest.FileMapping{
			{
				Filename:   "game.bin",
				Size:       uint64(len(data)),
				ShaContent: sha1Bytes(data),
				Chunks:     []*manifest.ChunkData{chunk},
			},
		},
	}

	state := storage.LoadState(tempDir)
	// Create downloader with nil servers (no network)
	d := engine.NewDownloader(nil, nil, state)

	// 1. Valid file: should pass verification without needing any chunk downloads
	opts := engine.DownloadOptions{
		OutputDir:       tempDir,
		VerifyIntegrity: true,
	}
	err = d.DownloadManifest(t.Context(), m, make([]byte, 32), opts)
	if err != nil {
		t.Fatalf("expected valid file to pass verification, got error: %v", err)
	}

	// 2. Corrupt the file on disk
	corruptData := []byte("Corrupted file content for testing verify integrity failure")
	if err := os.WriteFile(filePath, corruptData, 0644); err != nil {
		t.Fatal(err)
	}

	// Also simulate progress cache was saved from previous run
	tracker, _ := state.NewProgressTracker(12345, 99999)
	tracker.Record(chunk.ShaHex())
	tracker.Close()

	// 3. Run with VerifyIntegrity: true. It MUST detect corruption and NOT use resume cache!
	// It will attempt to download the repair chunk and fail because pool is nil, proving it detected corruption!
	err = d.DownloadManifest(t.Context(), m, make([]byte, 32), opts)
	if err == nil {
		t.Fatalf("expected corrupted file to be detected in -i mode, but it falsely succeeded")
	}
}
