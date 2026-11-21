package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"depotdownloader-go/pkg/crypto"
)

const (
	PayloadMagic   uint32 = 0x71F617D0
	MetadataMagic  uint32 = 0x1F4812BE
	SignatureMagic uint32 = 0x1B81B817
	EOFMagic       uint32 = 0x32C415AB
)

const (
	FlagUserConfig           uint32 = 1
	FlagVersionedUserConfig  uint32 = 2
	FlagEncrypted            uint32 = 4
	FlagReadOnly             uint32 = 8
	FlagHidden               uint32 = 16
	FlagExecutable           uint32 = 32
	FlagDirectory            uint32 = 64
	FlagCustomExecutable     uint32 = 128
	FlagInstallScript        uint32 = 256
	FlagSymlink              uint32 = 512
)

type ChunkData struct {
	Sha          []byte
	Crc          uint32
	Offset       uint64
	CbOriginal   uint32
	CbCompressed uint32
}

func (c *ChunkData) ShaHex() string {
	return fmt.Sprintf("%x", c.Sha)
}

type FileMapping struct {
	Filename    string
	Size        uint64
	Flags       uint32
	ShaFilename []byte
	ShaContent  []byte
	Chunks      []*ChunkData
	Linktarget  string
}

func (f *FileMapping) IsDirectory() bool {
	return f.Flags&FlagDirectory != 0
}

func (f *FileMapping) IsSymlink() bool {
	return f.Flags&FlagSymlink != 0
}

func (f *FileMapping) IsExecutable() bool {
	return (f.Flags&FlagExecutable != 0) || (f.Flags&FlagCustomExecutable != 0)
}

type ContentManifestMetadata struct {
	DepotID            uint32
	GidManifest        uint64
	CreationTime       uint32
	FilenamesEncrypted bool
	CbDiskOriginal     uint64
	CbDiskCompressed   uint64
	UniqueChunks       uint32
	CrcEncrypted       uint32
	CrcClear           uint32
}

type DepotManifest struct {
	Metadata           *ContentManifestMetadata
	Files              []*FileMapping
	FilenamesEncrypted bool
	RawData            []byte
}

// ParseFromFile loads and parses a .manifest file from disk.
func ParseFromFile(path string) (*DepotManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses the raw bytes of a Steam .manifest file.
func Parse(data []byte) (*DepotManifest, error) {
	r := bytes.NewReader(data)
	var payloadData []byte
	var metadataData []byte

	for {
		var magic uint32
		if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read magic error: %w", err)
		}

		if magic == EOFMagic {
			break
		}

		var length uint32
		if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
			return nil, fmt.Errorf("read length error: %w", err)
		}

		secData := make([]byte, length)
		if _, err := io.ReadFull(r, secData); err != nil {
			return nil, fmt.Errorf("read section payload error: %w", err)
		}

		switch magic {
		case PayloadMagic:
			payloadData = secData
		case MetadataMagic:
			metadataData = secData
		case SignatureMagic:
			// signature ignored
		default:
			// unrecognized magic
		}
	}

	if metadataData == nil {
		return nil, errors.New("missing metadata section in manifest")
	}

	meta, err := decodeMetadata(metadataData)
	if err != nil {
		return nil, fmt.Errorf("decode metadata error: %w", err)
	}

	var files []*FileMapping
	if payloadData != nil {
		files, err = decodePayload(payloadData)
		if err != nil {
			return nil, fmt.Errorf("decode payload error: %w", err)
		}
	}

	return &DepotManifest{
		Metadata:           meta,
		Files:              files,
		FilenamesEncrypted: meta.FilenamesEncrypted,
		RawData:            data,
	}, nil
}

// DecryptFilenames decrypts all filenames in the manifest using the 32-byte depot key.
func (m *DepotManifest) DecryptFilenames(depotKey []byte) error {
	if !m.FilenamesEncrypted {
		return nil
	}

	for _, f := range m.Files {
		decName, err := crypto.DecryptFilename(depotKey, f.Filename)
		if err != nil {
			return fmt.Errorf("failed to decrypt filename %q: %w", f.Filename, err)
		}
		f.Filename = decName

		if f.Linktarget != "" {
			decTarget, err := crypto.DecryptFilename(depotKey, f.Linktarget)
			if err != nil {
				return fmt.Errorf("failed to decrypt link target %q: %w", f.Linktarget, err)
			}
			f.Linktarget = decTarget
		}
	}

	m.FilenamesEncrypted = false
	return nil
}
