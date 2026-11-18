package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

type ChunkInfo struct {
	Offset       uint64
	OriginalSize uint32
	DepotID      uint32
	FileID       int
	ContentSHA   [20]byte
}

type DownloadedChunk struct {
	Info ChunkInfo
	Data []byte
}

type DepotConfig struct {
	ManifestPath string
	DepotKey     []byte
	OutputPath   string
	Retry        int
	Pattern      string
	CDNList      *cdnList
	Client       *http.Client
}

type fileTarget struct {
	Path      string
	Remaining int
}

type fileHandle struct {
	Path      string
	Remaining int64
}

var chunkDoneMu sync.Mutex
var chunkDirty bool

func doneMu(done map[string]map[string]bool, fn, key string) {
	chunkDoneMu.Lock()
	if done[fn] == nil {
		done[fn] = map[string]bool{}
	}
	done[fn][key] = true
	chunkDirty = true
	chunkDoneMu.Unlock()
}

func outputPathFor(output string, depotID uint32, name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Clean(name)
	if output == "default" {
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, fmt.Sprintf("%d", depotID), name)
	}
	return filepath.Join(output, name)
}

func loadChunkJSON(depotID uint32, dest *map[string]map[string]bool, path *string) {
	matches, _ := filepath.Glob(fmt.Sprintf(".depot_%d*.resume", depotID))
	if len(matches) > 0 {
		*path = matches[0]
		data, _ := os.ReadFile(*path)
		_ = json.Unmarshal(data, dest)
		if *dest == nil {
			*dest = map[string]map[string]bool{}
		}
		return
	}
	*path = fmt.Sprintf(".depot_%d.resume", depotID)
	*dest = map[string]map[string]bool{}
	b, _ := json.Marshal(*dest)
	os.WriteFile(*path, b, 0o644)
}

func saveChunkJSON(path string, m map[string]map[string]bool) {
	chunkDoneMu.Lock()
	defer chunkDoneMu.Unlock()
	b, _ := json.Marshal(m)
	tmp := path + ".tmp"
	os.WriteFile(tmp, b, 0o644)
	os.Rename(tmp, path)
	chunkDirty = false
}

func verifySHA1(r io.Reader, want []byte) (bool, error) {
	if len(want) != 20 {
		return false, fmt.Errorf("want sha1 len %d", len(want))
	}
	h := sha1.New()
	if _, err := io.Copy(h, r); err != nil {
		return false, err
	}
	return bytes.Equal(h.Sum(nil), want), nil
}

func prepareOutputFile(path string, wantSHA []byte, size uint64) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create dir %s: %w", dir, err)
	}
	st, err := os.Stat(path)
	if err == nil && uint64(st.Size()) == size {
		f, err := os.Open(path)
		if err == nil {
			ok, _ := verifySHA1(f, wantSHA)
			f.Close()
			if ok {
				log.Printf("%s already downloaded", path)
				return true, nil
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := f.Truncate(int64(size)); err != nil {
		return false, err
	}
	return false, nil
}

func RunDownloadManifest(ctx context.Context, cfg DepotConfig, manifest *DepotManifest) error {
	payload := manifest.Payload
	meta := manifest.Metadata

	var fileTargets []fileTarget
	var allChunks []ChunkInfo
	totalBytes := uint64(0)

	var re *regexp.Regexp
	if cfg.Pattern != "" {
		var err error
		re, err = regexp.Compile(cfg.Pattern)
		if err != nil {
			return fmt.Errorf("pattern: %w", err)
		}
	}

	for _, fm := range payload.Mappings {
		if fm.Flags&(FlagDirectory|FlagSymlink) != 0 {
			continue
		}
		name, err := meta.DecryptFileMappingFilename(&fm, cfg.DepotKey)
		if err != nil {
			log.Printf("skip file (decrypt err): %v", err)
			continue
		}
		if re != nil && !re.MatchString(filepath.ToSlash(name)) {
			continue
		}

		full := outputPathFor(cfg.OutputPath, meta.DepotID, name)
		exist, err := prepareOutputFile(full, fm.SHAContent, fm.Size)
		if err != nil {
			return err
		}
		if exist {
			continue
		}
		if len(fm.Chunks) == 0 {
			continue
		}

		fid := len(fileTargets)
		fileTargets = append(fileTargets, fileTarget{Path: full, Remaining: len(fm.Chunks)})
		for _, c := range fm.Chunks {
			if c.CBCompressed != 0 {
				totalBytes += uint64(c.CBCompressed)
			}
			if len(c.SHA) != 20 {
				return fmt.Errorf("%s: chunk sha len %d", name, len(c.SHA))
			}
			var sha [20]byte
			copy(sha[:], c.SHA)
			allChunks = append(allChunks, ChunkInfo{
				Offset:       c.Offset,
				OriginalSize: c.CBOriginal,
				DepotID:      meta.DepotID,
				FileID:       fid,
				ContentSHA:   sha,
			})
		}
	}

	if len(allChunks) == 0 {
		log.Println("nothing to download")
		return nil
	}

	bar := progressbar.NewOptions64(int64(totalBytes),
		progressbar.OptionSetDescription(fmt.Sprintf("Depot %d", meta.DepotID)),
		progressbar.OptionShowBytes(true),
	)
	defer bar.Finish()

	done := map[string]map[string]bool{}
	var chunkJSONPath string
	loadChunkJSON(meta.DepotID, &done, &chunkJSONPath)

	// 启动后台协程，每 2 秒自动写入断点
	stopAutoSave := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				chunkDoneMu.Lock()
				needsSave := chunkDirty
				chunkDoneMu.Unlock()
				if needsSave {
					saveChunkJSON(chunkJSONPath, done)
				}
			case <-stopAutoSave:
				return
			}
		}
	}()
	// 函数退出时，强保底保存一次
	defer func() {
		close(stopAutoSave)
		saveChunkJSON(chunkJSONPath, done)
	}()

	fileHandles := make([]*fileHandle, len(fileTargets))
	for i, ft := range fileTargets {
		fileHandles[i] = &fileHandle{Path: ft.Path, Remaining: int64(ft.Remaining)}
	}

	var todo []ChunkInfo
	for _, c := range allChunks {
		key := fmt.Sprintf("%d_%s", c.Offset, hex.EncodeToString(c.ContentSHA[:]))
		fn := fileTargets[c.FileID].Path
		if done[fn][key] {
			bar.Add64(int64(c.OriginalSize))
			atomic.AddInt64(&fileHandles[c.FileID].Remaining, -1)
			continue
		}
		todo = append(todo, c)
	}
	if len(todo) == 0 {
		log.Println("all chunks already downloaded")
		return nil
	}

	decodeConcurrency := runtime.NumCPU()
	if decodeConcurrency < 2 {
		decodeConcurrency = 2
	}
	downloadConcurrency := decodeConcurrency * 6
	if downloadConcurrency < 12 {
		downloadConcurrency = 12
	}
	if downloadConcurrency > 64 {
		downloadConcurrency = 64
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	queue := make(chan DownloadedChunk, decodeConcurrency*4)

	var decodeWg sync.WaitGroup
	var decodeErr atomic.Value
	var errOnce sync.Once
	setErr := func(e error) {
		errOnce.Do(func() {
			decodeErr.Store(e)
			cancel()
		})
	}

	for i := 0; i < decodeConcurrency; i++ {
		decodeWg.Add(1)
		go func() {
			defer decodeWg.Done()
			buf := make([]byte, 0, 4<<20)
			for dc := range queue {
				if err := decodeAndWriteChunk(dc, cfg.DepotKey, fileHandles, &buf); err != nil {
					setErr(err)
					for range queue {
					}
					return
				}
				key := fmt.Sprintf("%d_%s", dc.Info.Offset, hex.EncodeToString(dc.Info.ContentSHA[:]))
				fn := fileTargets[dc.Info.FileID].Path
				doneMu(done, fn, key)
			}
		}()
	}

	var dlWg sync.WaitGroup
	sem := make(chan struct{}, downloadConcurrency)
	for _, c := range todo {
		c := c
		dlWg.Add(1)
		go func() {
			defer dlWg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}
			data, err := fetchChunk(ctx, cfg.CDNList, cfg.Client, c, cfg.Retry)
			if err != nil {
				log.Printf("chunk %d failed: %v", c.Offset, err)
				setErr(err)
				return
			}
			bar.Add64(int64(c.OriginalSize))

			select {
			case queue <- DownloadedChunk{Info: c, Data: data}:
			case <-ctx.Done():
			}
		}()
	}

	go func() {
		dlWg.Wait()
		close(queue)
	}()

	decodeWg.Wait()

	if v := decodeErr.Load(); v != nil {
		return v.(error)
	}

	log.Println("正在将数据同步到磁盘...")
	for _, h := range fileHandles {
		if h.Remaining == 0 {
			if f, err := os.OpenFile(h.Path, os.O_RDWR, 0o644); err == nil {
				_ = f.Sync()
				f.Close()
			}
		}
	}

	log.Println("all downloads completed")
	return nil
}

func fetchChunk(ctx context.Context, cdn *cdnList, client *http.Client, c ChunkInfo, retry int) ([]byte, error) {
	chunkHex := hex.EncodeToString(c.ContentSHA[:])
	maxAttempts := retry + 1
	backoff := 100 * time.Millisecond
	avoid := -1
	var lastErr error

	for i := 0; i < maxAttempts; i++ {
		idx := cdn.pick(avoid)
		if idx < 0 {
			return nil, errors.New("no cdn host")
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		body, err := tryFetchOnce(reqCtx, client, cdn.urlFor(idx, c.DepotID, chunkHex))
		cancel()

		if err == nil {
			return body, nil
		}
		lastErr = err
		avoid = idx

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
	return nil, fmt.Errorf("chunk %s failed after %d attempts: %w", chunkHex, maxAttempts, lastErr)
}

func tryFetchOnce(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body from %s", url)
	}
	return body, nil
}

func decodeAndWriteChunk(dc DownloadedChunk, depotKey []byte, handles []*fileHandle, buf *[]byte) error {
	plain, err := decryptSteamBlob(dc.Data, depotKey)
	if err != nil {
		return fmt.Errorf("decrypt chunk %d: %w", dc.Info.Offset, err)
	}
	if err := decompressInto(plain, buf); err != nil {
		return fmt.Errorf("decompress chunk %d: %w", dc.Info.Offset, err)
	}
	if len(*buf) != int(dc.Info.OriginalSize) {
		return fmt.Errorf("size mismatch chunk %d: want %d got %d",
			dc.Info.Offset, dc.Info.OriginalSize, len(*buf))
	}

	h := handles[dc.Info.FileID]
	f, err := os.OpenFile(h.Path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteAt(*buf, int64(dc.Info.Offset)); err != nil {
		return err
	}

	atomic.AddInt64(&h.Remaining, -1)
	return nil
}