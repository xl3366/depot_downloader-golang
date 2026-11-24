package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"depotdownloader-go/pkg/crypto"
	"depotdownloader-go/pkg/decompress"
	"depotdownloader-go/pkg/manifest"
	"depotdownloader-go/pkg/steam"
	"depotdownloader-go/pkg/storage"
)

type DownloadOptions struct {
	AppID           uint32
	OutputDir       string
	ThreadNum       int
	RetryNum        int
	VerifyIntegrity bool
	DeleteUnmatched bool
	RegexPattern    string
}

type Downloader struct {
	serverPool   *steam.ServerPool
	tokenManager *steam.TokenManager
	stateStore   *storage.StateStore
	httpClient   *http.Client
}

func NewDownloader(pool *steam.ServerPool, tm *steam.TokenManager, state *storage.StateStore) *Downloader {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // We handle Steam chunk decompression ourselves
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	}

	return &Downloader{
		serverPool:   pool,
		tokenManager: tm,
		stateStore:   state,
		httpClient:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

type chunkWorkItem struct {
	depotID  uint32
	depotKey []byte
	chunk    *manifest.ChunkData
	file     *os.File
	filename string
}

func (d *Downloader) DownloadManifest(ctx context.Context, m *manifest.DepotManifest, depotKey []byte, opt DownloadOptions) error {
	depotID := m.Metadata.DepotID
	gid := m.Metadata.GidManifest
	log.Printf("[Downloader] Starting Depot %d (Manifest GID: %d, Files: %d)", depotID, gid, len(m.Files))

	outputDir := opt.OutputDir
	if outputDir == "" {
		outputDir = fmt.Sprintf("%d", depotID)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output dir %s: %w", outputDir, err)
	}

	// 1. Load previous installed manifest to detect incremental changes
	oldManifest, _ := d.stateStore.LoadInstalledManifest(depotID)
	oldFileMap := make(map[string]*manifest.FileMapping)
	if oldManifest != nil {
		for _, f := range oldManifest.Files {
			oldFileMap[f.Filename] = f
		}
		log.Printf("[Downloader] Found previously installed manifest GID %d, performing incremental comparison", oldManifest.Metadata.GidManifest)
	}

	var patternRegexp *regexp.Regexp
	if opt.RegexPattern != "" {
		re, err := regexp.Compile(opt.RegexPattern)
		if err != nil {
			return fmt.Errorf("invalid regex pattern: %w", err)
		}
		patternRegexp = re
	}

	// 2. Load progress cache (instant resume for HDD without scanning gigabytes of files)
	// ONLY use fast resume cache when -i (VerifyIntegrity) is FALSE!
	var completedChunks map[string]bool
	hasResumeCache := false
	if !opt.VerifyIntegrity {
		completedChunks = d.stateStore.LoadProgress(depotID, gid)
		hasResumeCache = len(completedChunks) > 0
		if hasResumeCache {
			log.Printf("[Downloader] Found resume cache with %d previously completed chunk(s). Fast resuming without disk scanning...", len(completedChunks))
		}
	} else {
		log.Printf("[Verify] Integrity verification mode enabled (-i): validating files on disk against manifest...")
	}
	progressTracker, err := d.stateStore.NewProgressTracker(depotID, gid)
	if err != nil {
		log.Printf("[Downloader] Warning: could not initialize progress tracker: %v", err)
	}
	if progressTracker != nil {
		defer progressTracker.Close()
	}

	// 3. Plan files and chunks
	var totalBytes uint64
	var pendingChunks []*chunkWorkItem
	var openFiles []*os.File
	defer func() {
		for _, f := range openFiles {
			_ = f.Close()
		}
	}()

	for _, fileEntry := range m.Files {
		cleanName := filepath.Clean(fileEntry.Filename)

		if patternRegexp != nil && !patternRegexp.MatchString(fileEntry.Filename) {
			continue
		}

		targetPath := filepath.Join(outputDir, cleanName)

		if fileEntry.IsDirectory() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		if fileEntry.IsSymlink() {
			_ = os.Remove(targetPath)
			if fileEntry.Linktarget != "" {
				_ = os.Symlink(fileEntry.Linktarget, targetPath)
			}
			continue
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		totalBytes += fileEntry.Size

		// Incremental / Already installed check:
		// If depot was already installed and committed with this manifest (or an older manifest where files match),
		// skip already completed files with matching size. ONLY if -i is not requested.
		fi, statErr := os.Stat(targetPath)
		if statErr == nil && uint64(fi.Size()) == fileEntry.Size && !opt.VerifyIntegrity {
			if oldManifest != nil {
				if oldManifest.Metadata.GidManifest == m.Metadata.GidManifest {
					// Fully installed at this manifest GID!
					continue
				}
				if oldFile, ok := oldFileMap[fileEntry.Filename]; ok {
					if bytes.Equal(oldFile.ShaContent, fileEntry.ShaContent) && len(fileEntry.ShaContent) > 0 {
						// Perfectly identical across different manifests, no need to touch
						continue
					}
				}
			}
		}

		// Fast file-level verification:
		// If verifying integrity (-i) and file exists with matching size and ShaContent:
		fileFullyValid := false
		if opt.VerifyIntegrity && statErr == nil && uint64(fi.Size()) == fileEntry.Size && len(fileEntry.ShaContent) > 0 {
			if err := verifyFileSha1(targetPath, fileEntry.ShaContent); err == nil {
				fileFullyValid = true
				if progressTracker != nil {
					for _, chunk := range fileEntry.Chunks {
						progressTracker.Record(chunk.ShaHex())
					}
				}
			} else {
				log.Printf("[Verify] File '%s' checksum mismatch or corrupted, checking chunk details for repair...", fileEntry.Filename)
			}
		}

		if fileFullyValid {
			if fileEntry.IsExecutable() {
				_ = os.Chmod(targetPath, 0755)
			}
			continue
		}

		// Open file for random writes
		f, err := os.OpenFile(targetPath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", targetPath, err)
		}
		openFiles = append(openFiles, f)

		// Set or truncate to expected size
		if statErr != nil || uint64(fi.Size()) != fileEntry.Size {
			if err := f.Truncate(int64(fileEntry.Size)); err != nil {
				return fmt.Errorf("failed to truncate %s: %w", targetPath, err)
			}
		}

		// Plan chunks: Check existing chunks for breakpoint resume or integrity repair
		for _, chunk := range fileEntry.Chunks {
			shaHex := chunk.ShaHex()
			needDownload := true

			if hasResumeCache {
				// Fast path 1: Instant memory cache lookup (0 disk I/O! 100% RAM, instant on HDD)
				if completedChunks[shaHex] {
					needDownload = false
				}
			} else if statErr == nil && fi.Size() >= int64(chunk.Offset+uint64(chunk.CbOriginal)) {
				// Fallback 2: On-disk physical Adler32 verification (when no resume cache or in -i mode)
				buf := make([]byte, chunk.CbOriginal)
				n, err := f.ReadAt(buf, int64(chunk.Offset))
				if err == nil && uint32(n) == chunk.CbOriginal {
					if crypto.Adler32Seed0(buf) == chunk.Crc {
						// Chunk already intact on disk!
						needDownload = false
						if progressTracker != nil {
							progressTracker.Record(shaHex)
						}
					}
				}
			}

			if needDownload {
				pendingChunks = append(pendingChunks, &chunkWorkItem{
					depotID:  depotID,
					depotKey: depotKey,
					chunk:    chunk,
					file:     f,
					filename: fileEntry.Filename,
				})
			}
		}

		if fileEntry.IsExecutable() {
			_ = os.Chmod(targetPath, 0755)
		}
	}

	log.Printf("[Downloader] Depot %d planned: Total size: %s, Chunks to download: %d",
		depotID, formatBytes(totalBytes), len(pendingChunks))

	if len(pendingChunks) == 0 {
		if opt.VerifyIntegrity {
			log.Printf("[Verify] Depot %d verification passed: 100%% validated! All files match manifest.", depotID)
		} else {
			log.Printf("[Downloader] Depot %d is already fully up to date! Committing state.", depotID)
		}
		if progressTracker != nil {
			progressTracker.Close()
			progressTracker = nil
		}
		_ = d.stateStore.CommitDepot(depotID, gid, m.RawData)
		return nil
	}

	if opt.VerifyIntegrity {
		log.Printf("[Verify] Depot %d verification detected %d missing/corrupted chunk(s). Downloading repair chunks...", depotID, len(pendingChunks))
	}

	// 4. Download chunks concurrently
	threadNum := opt.ThreadNum
	if threadNum <= 0 {
		threadNum = 32
	}
	if threadNum > len(pendingChunks) {
		threadNum = len(pendingChunks)
	}

	retryNum := opt.RetryNum
	if retryNum <= 0 {
		retryNum = 5
	}

	pb := newProgressBar(totalBytes)
	// Seed completed bytes for skipped chunks
	alreadyCompleted := totalBytes
	for _, item := range pendingChunks {
		if alreadyCompleted >= uint64(item.chunk.CbOriginal) {
			alreadyCompleted -= uint64(item.chunk.CbOriginal)
		}
	}
	pb.add(alreadyCompleted)

	chunkChan := make(chan *chunkWorkItem, len(pendingChunks))
	for _, item := range pendingChunks {
		chunkChan <- item
	}
	close(chunkChan)

	var wg sync.WaitGroup
	errChan := make(chan error, threadNum)

	for i := 0; i < threadNum; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range chunkChan {
				select {
				case <-ctx.Done():
					return
				default:
				}

				if err := d.downloadChunkWithRetry(ctx, item, opt.AppID, retryNum, pb); err != nil {
					select {
					case errChan <- err:
					default:
					}
					return
				}
				if progressTracker != nil {
					progressTracker.Record(item.chunk.ShaHex())
				}
				pb.add(uint64(item.chunk.CbOriginal))
			}
		}()
	}

	wg.Wait()
	close(errChan)
	pb.finish()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err, ok := <-errChan; ok {
		return fmt.Errorf("chunk download failed: %w", err)
	}

	// 5. Verification step if requested
	if opt.VerifyIntegrity {
		log.Printf("[Verify] Depot %d verifying repaired files...", depotID)
		for _, f := range m.Files {
			if f.IsDirectory() || f.IsSymlink() || len(f.ShaContent) == 0 {
				continue
			}
			if patternRegexp != nil && !patternRegexp.MatchString(f.Filename) {
				continue
			}
			targetPath := filepath.Join(outputDir, f.Filename)
			if err := verifyFileSha1(targetPath, f.ShaContent); err != nil {
				return fmt.Errorf("file verification failed for %s: %w", f.Filename, err)
			}
		}
		log.Printf("[Verify] Depot %d verification passed: all files match manifest checksums (100%% OK).", depotID)
	}

	// 6. Commit state to .DepotDownloader (only when full manifest was targeted)
	if patternRegexp == nil {
		if progressTracker != nil {
			progressTracker.Close()
		}
		if err := d.stateStore.CommitDepot(depotID, gid, m.RawData); err != nil {
			log.Printf("[Downloader] Warning: failed to commit state for depot %d: %v", depotID, err)
		}
	}

	log.Printf("[Downloader] Depot %d download completed successfully!", depotID)
	return nil
}

func (d *Downloader) downloadChunkWithRetry(ctx context.Context, item *chunkWorkItem, appID uint32, maxRetries int, pb *progressBar) error {
	var lastErr error
	shaHex := item.chunk.ShaHex()

	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if d.serverPool == nil {
			return fmt.Errorf("no server pool available")
		}

		server := d.serverPool.GetServer()
		u, _ := url.Parse(server)
		host := ""
		if u != nil {
			host = u.Host
		}

		reqAppID := appID
		if reqAppID == 0 {
			reqAppID = item.depotID
		}

		token, err := d.tokenManager.GetToken(reqAppID, item.depotID, host)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}

		reqURL := fmt.Sprintf("%s/depot/%d/chunk/%s%s", server, item.depotID, shaHex, token)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}

		resp, err := d.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}

			// Decrypt chunk
			decrypted, err := crypto.DecryptChunk(item.depotKey, data)
			if err != nil {
				lastErr = fmt.Errorf("chunk decrypt error: %w", err)
				continue
			}

			// Decompress & verify Adler32
			decompressed, err := decompress.DecompressAndVerify(decrypted, item.chunk.CbOriginal, item.chunk.Crc)
			if err != nil {
				lastErr = fmt.Errorf("chunk decompress/crc error: %w", err)
				continue
			}

			// Write directly to file at offset
			_, err = item.file.WriteAt(decompressed, int64(item.chunk.Offset))
			if err != nil {
				return fmt.Errorf("failed to write chunk at offset %d: %w", item.chunk.Offset, err)
			}

			return nil
		}

		_ = resp.Body.Close()

		// 401 or 403 Forbidden indicates token expiration or unauthorized server
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, server)
			d.serverPool.RemoveServer(server)
			d.tokenManager.InvalidateToken(reqAppID, item.depotID, host)
			time.Sleep(200 * time.Millisecond)
		} else {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, server)
		}
	}

	return fmt.Errorf("failed to download chunk %s after %d retries: %w", shaHex, maxRetries, lastErr)
}

// CleanupUnmatched scans outputDir and deletes files that are not in allowedFiles.
// It also cleans up any empty subdirectories leftover from deletions.
func CleanupUnmatched(outputDir string, allowedFiles map[string]bool) (int, error) {
	var deletedCount int
	var dirsToCheck []string

	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(outputDir, path)
		if err != nil {
			return nil
		}
		relClean := filepath.Clean(rel)
		if relClean == "." {
			return nil
		}
		if relClean == storage.StateDirName || strings.HasPrefix(relClean, storage.StateDirName+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			dirsToCheck = append(dirsToCheck, path)
			return nil
		}

		isAllowed := allowedFiles[relClean]
		if !isAllowed && runtime.GOOS == "windows" {
			isAllowed = allowedFiles[strings.ToLower(relClean)]
		}

		if !isAllowed {
			log.Printf("[Cleanup] Removing unmatched file: %s", relClean)
			if rmErr := os.Remove(path); rmErr == nil {
				deletedCount++
			}
		}
		return nil
	})

	if err != nil {
		return deletedCount, err
	}

	// Remove empty subdirectories (deepest first)
	sort.Slice(dirsToCheck, func(i, j int) bool {
		return len(dirsToCheck[i]) > len(dirsToCheck[j])
	})

	for _, dir := range dirsToCheck {
		rel, err := filepath.Rel(outputDir, dir)
		if err != nil {
			continue
		}
		relClean := filepath.Clean(rel)
		if relClean == "." || relClean == storage.StateDirName || strings.HasPrefix(relClean, storage.StateDirName+string(filepath.Separator)) {
			continue
		}
		isAllowed := allowedFiles[relClean]
		if !isAllowed && runtime.GOOS == "windows" {
			isAllowed = allowedFiles[strings.ToLower(relClean)]
		}
		if isAllowed {
			continue
		}
		_ = os.Remove(dir)
	}

	return deletedCount, nil
}

func verifyFileSha1(path string, expectedSha []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha1.New()
	buf := make([]byte, 2*1024*1024)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return err
	}

	actualSha := h.Sum(nil)
	if !bytes.Equal(actualSha, expectedSha) {
		return fmt.Errorf("SHA-1 mismatch: expected %x, got %x", expectedSha, actualSha)
	}
	return nil
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Simple and smooth CLI Progress Bar
type progressBar struct {
	total     uint64
	current   atomic.Uint64
	startTime time.Time
	lastTime  time.Time
	lastBytes uint64
	stopChan  chan struct{}
}

func newProgressBar(total uint64) *progressBar {
	pb := &progressBar{
		total:     total,
		startTime: time.Now(),
		lastTime:  time.Now(),
		stopChan:  make(chan struct{}),
	}
	go pb.renderLoop()
	return pb
}

func (pb *progressBar) add(bytes uint64) {
	pb.current.Add(bytes)
}

func (pb *progressBar) finish() {
	close(pb.stopChan)
	pb.render(true)
}

func (pb *progressBar) renderLoop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-pb.stopChan:
			return
		case <-ticker.C:
			pb.render(false)
		}
	}
}

func (pb *progressBar) render(done bool) {
	curr := pb.current.Load()
	if curr > pb.total {
		curr = pb.total
	}

	now := time.Now()
	elapsed := now.Sub(pb.startTime).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}

	speed := float64(curr) / elapsed

	var pct float64
	if pb.total > 0 {
		pct = float64(curr) / float64(pb.total) * 100
	}

	barWidth := 30
	completedWidth := int(float64(barWidth) * (pct / 100))
	if completedWidth > barWidth {
		completedWidth = barWidth
	}

	bar := strings.Repeat("=", completedWidth)
	if completedWidth < barWidth {
		bar += ">" + strings.Repeat(" ", barWidth-completedWidth-1)
	}

	fmt.Printf("\r[%s] %.1f%% %s/%s (%.2f MB/s)",
		bar, pct, formatBytes(curr), formatBytes(pb.total), speed/(1024*1024))

	if done {
		fmt.Println()
	}
}
