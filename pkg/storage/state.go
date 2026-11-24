package storage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"depotdownloader-go/pkg/manifest"
)

const (
	StateDirName  = ".DepotDownloader"
	StateFileName = "depots.json"
)

type DepotState struct {
	Gid         uint64 `json:"gid"`
	UpdatedUnix int64  `json:"updated_unix"`
}

type StateFile struct {
	Version uint32                `json:"version"`
	Depots  map[string]DepotState `json:"depots"`
}

type StateStore struct {
	rootDir   string
	statePath string
	state     StateFile
}

func LoadState(outputDir string) *StateStore {
	root := filepath.Join(outputDir, StateDirName)
	statePath := filepath.Join(root, StateFileName)

	store := &StateStore{
		rootDir:   root,
		statePath: statePath,
		state: StateFile{
			Version: 1,
			Depots:  make(map[string]DepotState),
		},
	}

	data, err := os.ReadFile(statePath)
	if err == nil {
		_ = json.Unmarshal(data, &store.state)
		if store.state.Depots == nil {
			store.state.Depots = make(map[string]DepotState)
		}
	}

	return store
}

func (s *StateStore) GetInstalledGid(depotID uint32) (uint64, bool) {
	state, ok := s.state.Depots[fmt.Sprintf("%d", depotID)]
	if !ok {
		return 0, false
	}
	return state.Gid, true
}

func (s *StateStore) GetInstalledDepots() []uint32 {
	var depots []uint32
	for k := range s.state.Depots {
		var id uint32
		if _, err := fmt.Sscanf(k, "%d", &id); err == nil {
			depots = append(depots, id)
		}
	}
	return depots
}

func (s *StateStore) ManifestCachePath(depotID uint32, gid uint64) string {
	return filepath.Join(s.rootDir, fmt.Sprintf("%d_%d.manifest", depotID, gid))
}

func (s *StateStore) LoadInstalledManifest(depotID uint32) (*manifest.DepotManifest, error) {
	gid, ok := s.GetInstalledGid(depotID)
	if !ok {
		return nil, nil
	}
	cachePath := s.ManifestCachePath(depotID, gid)
	return manifest.ParseFromFile(cachePath)
}

func (s *StateStore) CommitDepot(depotID uint32, gid uint64, rawManifest []byte) error {
	if err := os.MkdirAll(s.rootDir, 0755); err != nil {
		return err
	}

	// 1. Cache current manifest
	cachePath := s.ManifestCachePath(depotID, gid)
	if len(rawManifest) > 0 {
		_ = os.WriteFile(cachePath, rawManifest, 0644)
	}

	// 2. Remove obsolete manifest and progress files for this depot
	entries, _ := os.ReadDir(s.rootDir)
	prefix := fmt.Sprintf("%d_", depotID)
	currentName := filepath.Base(cachePath)
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			if filepath.Ext(name) == ".manifest" && name != currentName {
				_ = os.Remove(filepath.Join(s.rootDir, name))
			}
			if filepath.Ext(name) == ".progress" {
				_ = os.Remove(filepath.Join(s.rootDir, name))
			}
		}
	}

	// 3. Update depots.json
	s.state.Depots[fmt.Sprintf("%d", depotID)] = DepotState{
		Gid:         gid,
		UpdatedUnix: time.Now().Unix(),
	}

	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := s.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	_ = os.Remove(s.ProgressFilePath(depotID, gid))
	return os.Rename(tmpPath, s.statePath)
}

func (s *StateStore) ProgressFilePath(depotID uint32, gid uint64) string {
	return filepath.Join(s.rootDir, fmt.Sprintf("%d_%d.progress", depotID, gid))
}

func (s *StateStore) LoadProgress(depotID uint32, gid uint64) map[string]bool {
	completed := make(map[string]bool)
	path := s.ProgressFilePath(depotID, gid)
	f, err := os.Open(path)
	if err != nil {
		return completed
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			completed[line] = true
		}
	}
	return completed
}

type ProgressTracker struct {
	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	count int
	path  string
}

func (s *StateStore) NewProgressTracker(depotID uint32, gid uint64) (*ProgressTracker, error) {
	if err := os.MkdirAll(s.rootDir, 0755); err != nil {
		return nil, err
	}
	path := s.ProgressFilePath(depotID, gid)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &ProgressTracker{
		f:    f,
		w:    bufio.NewWriterSize(f, 32*1024),
		path: path,
	}, nil
}

func (pt *ProgressTracker) Record(shaHex string) {
	if pt == nil || shaHex == "" {
		return
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()

	_, _ = pt.w.WriteString(shaHex)
	_ = pt.w.WriteByte('\n')
	pt.count++
	if pt.count%10 == 0 {
		_ = pt.w.Flush()
	}
}

func (pt *ProgressTracker) Close() {
	if pt == nil || pt.f == nil {
		return
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	_ = pt.w.Flush()
	_ = pt.f.Close()
	pt.f = nil
}

func (pt *ProgressTracker) Remove() {
	if pt == nil {
		return
	}
	pt.Close()
	_ = os.Remove(pt.path)
}
