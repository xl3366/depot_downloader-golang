package main

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

type DepotManifest struct {
	DepotID    uint32
	ManifestID uint64
	Payload    *ContentManifestPayload
	Metadata   *ContentManifestMetadata
}

const (
	FlagDirectory = 1 << 6
	FlagSymlink   = 1 << 9
)

const (
	MagicPayload       = 0x71F617D0
	MagicMetadata      = 0x1F4812BE
	MagicSignature     = 0x1B81B817
	MagicEndOfManifest = 0x32C415AB
)

type ContentManifestPayload struct {
	Mappings []FileMapping
}

type FileMapping struct {
	Filename    string
	Size        uint64
	Flags       uint32
	SHAFilename []byte
	SHAContent  []byte
	Chunks      []ChunkData
	LinkTarget  string
}

type ChunkData struct {
	SHA          []byte
	CRC          uint32
	Offset       uint64
	CBOriginal   uint32
	CBCompressed uint32
}

type ContentManifestMetadata struct {
	DepotID            uint32
	GIDManifest        uint64
	CreationTime       uint32
	FilenamesEncrypted bool
	CBDiskOriginal     uint64
	CBDiskCompressed   uint64
	UniqueChunks       uint32
	CRCEncrypted       uint32
	CRCClear           uint32
}

func DeserializeManifest(raw []byte) (*ContentManifestPayload, *ContentManifestMetadata, error) {
	if len(raw) > 4 && bytes.HasPrefix(raw, []byte{0x50, 0x4B, 0x03, 0x04}) {
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err == nil && len(zr.File) > 0 {
			rc, err := zr.File[0].Open()
			if err == nil {
				defer rc.Close()
				uncompressed, err := io.ReadAll(rc)
				if err == nil {
					raw = uncompressed
				}
			}
		}
	}

	buf := bytes.NewReader(raw)

	var magic, length uint32
	if err := binary.Read(buf, binary.LittleEndian, &magic); err != nil { return nil, nil, err }
	if err := binary.Read(buf, binary.LittleEndian, &length); err != nil { return nil, nil, err }
	if magic != MagicPayload {
		return nil, nil, fmt.Errorf("expecting protobuf payload magic 0x%X, got 0x%X", MagicPayload, magic)
	}
	payloadBytes := make([]byte, length)
	if _, err := io.ReadFull(buf, payloadBytes); err != nil { return nil, nil, err }
	
	payload, err := parsePayload(payloadBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("payload parse error: %w", err)
	}

	if err := binary.Read(buf, binary.LittleEndian, &magic); err != nil { return nil, nil, err }
	if err := binary.Read(buf, binary.LittleEndian, &length); err != nil { return nil, nil, err }
	if magic != MagicMetadata {
		return nil, nil, fmt.Errorf("expecting protobuf metadata magic 0x%X, got 0x%X", MagicMetadata, magic)
	}
	metaBytes := make([]byte, length)
	if _, err := io.ReadFull(buf, metaBytes); err != nil { return nil, nil, err }

	meta, err := parseMetadata(metaBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata parse error: %w", err)
	}

	return payload, meta, nil
}

func parsePayload(b []byte) (*ContentManifestPayload, error) {
	p := &ContentManifestPayload{}
	for len(b) > 0 {
		field, wire, val, rest, err := readTag(b)
		if err != nil { return nil, err }
		b = rest
		if field == 1 && wire == 2 {
			fm, err := parseFileMapping(val)
			if err != nil { return nil, err }
			p.Mappings = append(p.Mappings, *fm)
		}
	}
	return p, nil
}

func parseFileMapping(b []byte) (*FileMapping, error) {
	f := &FileMapping{}
	for len(b) > 0 {
		field, wire, val, rest, err := readTag(b)
		if err != nil { return nil, err }
		b = rest
		switch {
		case field == 1 && wire == 2:
			f.Filename = string(val)
		case field == 2 && wire == 0:
			f.Size, _ = binary.Uvarint(val)
		case field == 3 && wire == 0:
			v, _ := binary.Uvarint(val)
			f.Flags = uint32(v)
		case field == 4 && wire == 2:
			f.SHAFilename = append([]byte{}, val...)
		case field == 5 && wire == 2:
			f.SHAContent = append([]byte{}, val...)
		case field == 6 && wire == 2:
			c, err := parseChunkData(val)
			if err != nil { return nil, err }
			f.Chunks = append(f.Chunks, *c)
		case field == 7 && wire == 2:
			f.LinkTarget = string(val)
		}
	}
	return f, nil
}

func parseChunkData(b []byte) (*ChunkData, error) {
	c := &ChunkData{}
	for len(b) > 0 {
		field, wire, val, rest, err := readTag(b)
		if err != nil { return nil, err }
		b = rest
		switch {
		case field == 1 && wire == 2:
			c.SHA = append([]byte{}, val...)
		case field == 2 && wire == 0:
			v, _ := binary.Uvarint(val)
			c.CRC = uint32(v)
		case field == 3 && wire == 0:
			c.Offset, _ = binary.Uvarint(val)
		case field == 4 && wire == 0:
			v, _ := binary.Uvarint(val)
			c.CBOriginal = uint32(v)
		case field == 5 && wire == 0:
			v, _ := binary.Uvarint(val)
			c.CBCompressed = uint32(v)
		}
	}
	return c, nil
}

func parseMetadata(b []byte) (*ContentManifestMetadata, error) {
	m := &ContentManifestMetadata{}
	for len(b) > 0 {
		field, wire, val, rest, err := readTag(b)
		if err != nil { return nil, err }
		b = rest
		if wire != 0 { continue }
		v, _ := binary.Uvarint(val)
		switch field {
		case 1: m.DepotID = uint32(v)
		case 2: m.GIDManifest = v
		case 3: m.CreationTime = uint32(v)
		case 4: m.FilenamesEncrypted = v != 0
		case 5: m.CBDiskOriginal = v
		case 6: m.CBDiskCompressed = v
		case 7: m.UniqueChunks = uint32(v)
		case 8: m.CRCEncrypted = uint32(v)
		case 9: m.CRCClear = uint32(v)
		}
	}
	return m, nil
}

func readTag(b []byte) (uint64, uint64, []byte, []byte, error) {
	key, n := binary.Uvarint(b)
	if n <= 0 { return 0, 0, nil, nil, errors.New("bad varint key") }
	b = b[n:]
	field := key >> 3
	wire := key & 7
	switch wire {
	case 0:
		v, n := binary.Uvarint(b)
		if n <= 0 { return 0, 0, nil, nil, errors.New("bad varint value") }
		var buf [10]byte
		m := binary.PutUvarint(buf[:], v)
		return field, wire, buf[:m], b[n:], nil
	case 1:
		if len(b) < 8 { return 0, 0, nil, nil, errors.New("short fixed64") }
		return field, wire, b[:8], b[8:], nil
	case 2:
		l, n := binary.Uvarint(b)
		if n <= 0 || uint64(len(b)-n) < l { return 0, 0, nil, nil, errors.New("bad len") }
		return field, wire, b[n : n+int(l)], b[n+int(l):], nil
	case 5:
		if len(b) < 4 { return 0, 0, nil, nil, errors.New("short fixed32") }
		return field, wire, b[:4], b[4:], nil
	}
	return 0, 0, nil, nil, fmt.Errorf("unsupported wire type %d", wire)
}

// 注意：这里只过滤控制字符和 Windows 绝对不允许的特殊符号
// 绝对不能把 / 和 \ 过滤掉，否则所有文件夹会被挤压成一根棍子
func sanitizeFilename(s string) string {
	var b strings.Builder
	invalidChars := map[rune]bool{
		':': true, '*': true, '?': true, '"': true, '<': true, '>': true, '|': true,
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || invalidChars[r] {
			continue
		}
		b.WriteRune(r)
	}
	res := b.String()
	res = strings.ReplaceAll(res, "..", "") // 防止路径穿越
	return res
}

func (m *ContentManifestMetadata) DecryptFileMappingFilename(fm *FileMapping, depotKey []byte) (string, error) {
	if !m.FilenamesEncrypted {
		return sanitizeFilename(fm.Filename), nil
	}
	enc, err := base64.StdEncoding.DecodeString(fm.Filename)
	if err != nil { return "", fmt.Errorf("base64 filename: %w", err) }
	plain, err := decryptSteamBlob(enc, depotKey)
	if err != nil { return "", err }
	return sanitizeFilename(string(plain)), nil
}

func decryptSteamBlob(data, key []byte) ([]byte, error) {
	if len(data) < 16 { return nil, errors.New("encrypted blob too short") }
	if len(key) != 32 { return nil, fmt.Errorf("depot key must be 32 bytes, got %d", len(key)) }
	block, err := aes.NewCipher(key)
	if err != nil { return nil, err }

	iv := make([]byte, 16)
	copy(iv, data[:16])
	block.Decrypt(iv, iv)

	ct := data[16:]
	if len(ct) == 0 || len(ct)%16 != 0 { return nil, errors.New("ciphertext not block aligned") }
	out := make([]byte, len(ct))
	prev := iv
	for i := 0; i < len(ct); i += 16 {
		block.Decrypt(out[i:i+16], ct[i:i+16])
		for j := 0; j < 16; j++ {
			out[i+j] ^= prev[j]
		}
		prev = ct[i : i+16]
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > 16 || pad > len(out) { return nil, errors.New("bad pkcs7 padding") }
	return out[:len(out)-pad], nil
}