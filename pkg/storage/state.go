package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	// 2. Remove obsolete manifest cache files for this depot
	entries, _ := os.ReadDir(s.rootDir)
	prefix := fmt.Sprintf("%d_", depotID)
	currentName := filepath.Base(cachePath)
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > len(prefix) && name[:len(prefix)] == prefix && filepath.Ext(name) == ".manifest" {
			if name != currentName {
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
	return os.Rename(tmpPath, s.statePath)
}
