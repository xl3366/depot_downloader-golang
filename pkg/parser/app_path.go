package parser

import (
	"archive/zip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"depotdownloader-go/pkg/manifest"
)

var (
	// Matches: addappid(813230, 1, "0a27e705...") -- ANIMAL WELL
	luaAddAppIDRe = regexp.MustCompile(`(?m)^[ \t]*addappid[ \t]*\([ \t]*(\d+)[ \t]*(?:,[ \t]*\d+[ \t]*)?(?:,[ \t]*"([0-9a-fA-F]*)"[ \t]*)?\)[ \t]*(?:--[ \t]*(.*?))?[ \t]*$`)
)

type AppPackageInfo struct {
	AppID     uint32
	DepotKeys map[uint32][]byte
	Manifests []*manifest.DepotManifest
}

// ParseAppPath scans a directory or a .zip archive for *.manifest, *.lua, and config.vdf.
func ParseAppPath(path string) (*AppPackageInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !fi.IsDir() {
		if strings.EqualFold(filepath.Ext(path), ".zip") {
			return ParseZipPath(path)
		}
		return nil, fmt.Errorf("%q is neither a directory nor a .zip archive", path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	info := &AppPackageInfo{
		DepotKeys: make(map[uint32][]byte),
	}

	var manifestFiles []string
	var luaFiles []string
	var vdfFiles []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		fullPath := filepath.Join(path, name)

		switch ext {
		case ".manifest":
			manifestFiles = append(manifestFiles, fullPath)
		case ".lua":
			luaFiles = append(luaFiles, fullPath)
		case ".vdf":
			vdfFiles = append(vdfFiles, fullPath)
		}
	}

	// 1. Parse Lua files first
	for _, luaPath := range luaFiles {
		appID, keys, err := ParseLuaKeys(luaPath)
		if err != nil {
			return nil, fmt.Errorf("error parsing lua file %s: %w", luaPath, err)
		}
		if info.AppID == 0 && appID != 0 {
			info.AppID = appID
		}
		for k, v := range keys {
			info.DepotKeys[k] = v
		}
	}

	// 2. Parse VDF files (VDF takes priority over lua if duplicate)
	for _, vdfPath := range vdfFiles {
		keys, err := ParseVDFKeys(vdfPath)
		if err != nil {
			return nil, fmt.Errorf("error parsing vdf file %s: %w", vdfPath, err)
		}
		for k, v := range keys {
			info.DepotKeys[k] = v
		}
	}

	// 3. Parse manifest files
	sort.Strings(manifestFiles)
	for _, mPath := range manifestFiles {
		m, err := manifest.ParseFromFile(mPath)
		if err != nil {
			return nil, fmt.Errorf("error parsing manifest %s: %w", mPath, err)
		}

		// Decrypt filenames if key is available
		if m.FilenamesEncrypted {
			key, ok := info.DepotKeys[m.Metadata.DepotID]
			if !ok {
				return nil, fmt.Errorf("missing depot key for depot %d (manifest: %s)", m.Metadata.DepotID, filepath.Base(mPath))
			}
			if err := m.DecryptFilenames(key); err != nil {
				return nil, fmt.Errorf("error decrypting filenames for depot %d: %w", m.Metadata.DepotID, err)
			}
		}

		info.Manifests = append(info.Manifests, m)
	}

	if len(info.Manifests) == 0 {
		return nil, errors.New("no .manifest files found in directory")
	}

	return info, nil
}

// ParseZipPath inspects and parses manifests, lua and vdf from a zip archive directly.
func ParseZipPath(zipPath string) (*AppPackageInfo, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}
	defer zr.Close()

	info := &AppPackageInfo{
		DepotKeys: make(map[uint32][]byte),
	}

	type rawManifest struct {
		name string
		data []byte
	}
	var rawManifests []rawManifest

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		switch ext {
		case ".lua":
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			appID, keys := ParseLuaData(data)
			if info.AppID == 0 && appID != 0 {
				info.AppID = appID
			}
			for k, v := range keys {
				info.DepotKeys[k] = v
			}
		case ".vdf":
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			keys := ParseVDFData(data)
			for k, v := range keys {
				info.DepotKeys[k] = v
			}
		case ".manifest":
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			rawManifests = append(rawManifests, rawManifest{name: f.Name, data: data})
		}
	}

	for _, item := range rawManifests {
		m, err := manifest.Parse(item.data)
		if err != nil {
			return nil, fmt.Errorf("error parsing manifest %s in zip: %w", item.name, err)
		}
		if m.FilenamesEncrypted {
			key, ok := info.DepotKeys[m.Metadata.DepotID]
			if !ok {
				return nil, fmt.Errorf("missing depot key for depot %d (manifest %s in zip)", m.Metadata.DepotID, item.name)
			}
			if err := m.DecryptFilenames(key); err != nil {
				return nil, fmt.Errorf("error decrypting filenames for depot %d: %w", m.Metadata.DepotID, err)
			}
		}
		info.Manifests = append(info.Manifests, m)
	}

	if len(info.Manifests) == 0 {
		return nil, errors.New("no .manifest files found in zip archive")
	}

	return info, nil
}

// ParseLuaKeys extracts AppID and Depot keys from a steam appinfo Lua file.
func ParseLuaKeys(luaPath string) (uint32, map[uint32][]byte, error) {
	data, err := os.ReadFile(luaPath)
	if err != nil {
		return 0, nil, err
	}
	appID, keys := ParseLuaData(data)
	return appID, keys, nil
}

// ParseLuaData extracts AppID and Depot keys from Lua content bytes.
func ParseLuaData(data []byte) (uint32, map[uint32][]byte) {
	var appID uint32
	keys := make(map[uint32][]byte)

	matches := luaAddAppIDRe.FindAllStringSubmatch(string(data), -1)
	for _, match := range matches {
		idVal, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil {
			continue
		}
		depotID := uint32(idVal)
		if appID == 0 {
			appID = depotID
		}

		keyHex := strings.TrimSpace(match[2])
		if len(keyHex) == 64 {
			keyBytes, err := hex.DecodeString(keyHex)
			if err == nil && len(keyBytes) == 32 {
				keys[depotID] = keyBytes
			}
		}
	}

	return appID, keys
}

// ParseVDFKeys parses keys from a KeyValues / config.vdf file.
func ParseVDFKeys(vdfPath string) (map[uint32][]byte, error) {
	data, err := os.ReadFile(vdfPath)
	if err != nil {
		return nil, err
	}
	return ParseVDFData(data), nil
}

// ParseVDFData parses keys from KeyValues content bytes.
func ParseVDFData(data []byte) map[uint32][]byte {
	keys := make(map[uint32][]byte)
	content := string(data)

	reDepot := regexp.MustCompile(`"(\d+)"\s*\{[^}]*?"DecryptionKey"\s*"([0-9a-fA-F]{64})"`)
	matches := reDepot.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		idVal, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		keyBytes, err := hex.DecodeString(m[2])
		if err == nil && len(keyBytes) == 32 {
			keys[uint32(idVal)] = keyBytes
		}
	}

	return keys
}
