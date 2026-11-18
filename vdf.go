package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/andygrunwald/vdf"
)

// 匹配 lua 中的 addappid(123456, 1, "key")
var luaAddAppIDRe = regexp.MustCompile(`(?m)^[ \t]*addappid[ \t]*\([ \t]*(\d+)[ \t]*(?:,[ \t]*\d+[ \t]*)?(?:,[ \t]*"([0-9a-fA-F]*)"[ \t]*)?\)`)

// ParseAppDir 解析包含 lua/acf 和 manifest 的目录
func ParseAppDir(dir string) (map[uint32]*DepotManifest, map[uint32][]byte, uint32, error) {
	manifests := make(map[uint32]*DepotManifest)
	depotKeys := make(map[uint32][]byte)
	var appID uint32

	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, 0, err
	}

	// 1. 找 lua / acf / vdf 文件，解析 AppID 和 depot keys
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name()))
		path := filepath.Join(dir, f.Name())

		// ---- 解析 lua ----
		if ext == ".lua" {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			matches := luaAddAppIDRe.FindAllStringSubmatch(string(data), -1)
			for _, match := range matches {
				depotID64, _ := strconv.ParseUint(match[1], 10, 32)
				id := uint32(depotID64)
				if appID == 0 {
					appID = id // 第一个通常是 appid
				}
				if match[2] != "" {
					keyBytes, err := hex.DecodeString(match[2])
					if err == nil {
						depotKeys[id] = keyBytes
					}
				}
			}
		}

		// ---- 解析 acf / vdf ----
		if ext == ".acf" || ext == ".vdf" {
			file, err := os.Open(path)
			if err != nil {
				continue
			}
			parser := vdf.NewParser(file)
			data, err := parser.Parse()
			file.Close()
			if err != nil {
				continue
			}

			for k, v := range data {
				appData, ok := v.(map[string]interface{})
				if !ok {
					continue
				}

				if appidStr, ok := appData["appid"].(string); ok {
					id, _ := strconv.ParseUint(appidStr, 10, 32)
					appID = uint32(id)
				} else if id, err := strconv.ParseUint(k, 10, 32); err == nil {
					appID = uint32(id)
				}

				if depots, ok := appData["depots"].(map[string]interface{}); ok {
					for depotIDStr, depotData := range depots {
						depotID64, err := strconv.ParseUint(depotIDStr, 10, 32)
						if err != nil {
							continue
						}
						id := uint32(depotID64)
						if dMap, ok := depotData.(map[string]interface{}); ok {
							if key, ok := dMap["DecryptionKey"].(string); ok {
								keyBytes, err := hex.DecodeString(key)
								if err == nil {
									depotKeys[id] = keyBytes
								}
							}
						}
					}
				}
			}
		}
	}

	// 2. 找 manifest 文件并加载
	for _, f := range files {
		if f.IsDir() || strings.ToLower(filepath.Ext(f.Name())) != ".manifest" {
			continue
		}

		// 从文件名提取 depot id (例如 5335511_43933022531591698.manifest)
		parts := strings.Split(f.Name(), "_")
		if len(parts) < 1 {
			continue
		}
		depotID64, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			continue
		}
		depotID := uint32(depotID64)

		path := filepath.Join(dir, f.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("读取 manifest 失败 %s: %w", f.Name(), err)
		}

		payload, meta, err := DeserializeManifest(raw)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("解析 manifest 失败 %s: %w", f.Name(), err)
		}

		manifests[depotID] = &DepotManifest{
			DepotID:    depotID,
			ManifestID: meta.GIDManifest,
			Payload:    payload,
			Metadata:   meta,
		}
	}

	// 3. 双向比对，输出警告
	for depotID := range depotKeys {
		if _, ok := manifests[depotID]; !ok {
			fmt.Printf("[警告] Lua/ACF 里存在 Depot %d 的 Key，但目录里没找到对应的 .manifest 文件，已跳过。\n", depotID)
		}
	}
	for depotID := range manifests {
		if _, ok := depotKeys[depotID]; !ok {
			fmt.Printf("[警告] 找到了 Depot %d 的 .manifest，但未找到解密 Key。请使用 -k 手动指定，否则将跳过此 Depot。\n", depotID)
		}
	}

	if len(manifests) == 0 {
		return nil, nil, 0, fmt.Errorf("未在目录 %s 找到任何有效的 .manifest 文件", dir)
	}

	return manifests, depotKeys, appID, nil
}