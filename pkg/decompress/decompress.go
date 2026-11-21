package decompress

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"depotdownloader-go/pkg/crypto"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"
)

var (
	zstdDecoder *zstd.Decoder
)

func init() {
	var err error
	zstdDecoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		panic(err)
	}
}

// DecompressAndVerify decompresses a decrypted chunk and checks expected length & Adler32 checksum.
func DecompressAndVerify(decrypted []byte, expectedLen uint32, expectedCrc uint32) ([]byte, error) {
	var out []byte
	var err error

	if bytes.HasPrefix(decrypted, []byte("VSZa")) {
		out, err = decompressVZstd(decrypted)
	} else if bytes.HasPrefix(decrypted, []byte("VZa")) {
		out, err = decompressVZip(decrypted)
	} else if bytes.HasPrefix(decrypted, []byte("PK\x03\x04")) {
		out, err = decompressZip(decrypted)
	} else {
		return nil, fmt.Errorf("unknown compression format (magic: %q)", decrypted[:min(4, len(decrypted))])
	}

	if err != nil {
		return nil, err
	}

	if uint32(len(out)) != expectedLen {
		return nil, fmt.Errorf("decompressed size mismatch: got %d, expected %d", len(out), expectedLen)
	}

	actualCrc := crypto.Adler32Seed0(out)
	if actualCrc != expectedCrc {
		return nil, fmt.Errorf("adler32 checksum mismatch: got 0x%08X, expected 0x%08X", actualCrc, expectedCrc)
	}

	return out, nil
}

// decompressVZstd: header(8) + frame(>=1) + footer(15)
// header: "VSZa"(4) + crc32(4)
// footer: crc32(4) + decompressed_size(4) + crc32(4) + "zsv"(3)
func decompressVZstd(data []byte) ([]byte, error) {
	if len(data) < 8+15+1 {
		return nil, errors.New("VZstd data too short")
	}
	if !bytes.HasSuffix(data, []byte("zsv")) {
		return nil, errors.New("missing VZstd footer")
	}

	total := len(data)
	decompressedSize := binary.LittleEndian.Uint32(data[total-11 : total-7])
	frame := data[8 : total-15]

	dst := make([]byte, 0, decompressedSize)
	out, err := zstdDecoder.DecodeAll(frame, dst)
	if err != nil {
		return nil, fmt.Errorf("zstd decode error: %w", err)
	}
	if uint32(len(out)) != decompressedSize {
		return nil, fmt.Errorf("zstd decompressed size mismatch: got %d, expected %d", len(out), decompressedSize)
	}
	return out, nil
}

// decompressVZip: header(7) + props(5) + lzma(>=1) + footer(10)
// header: "VZa"(3) + timestamp/crc(4)
// props: 1 byte lc/lp/pb + 4 bytes dict_size
// footer: crc32(4) + size(4) + "zv"(2)
func decompressVZip(data []byte) ([]byte, error) {
	if len(data) < 7+5+1+10 {
		return nil, errors.New("VZip data too short")
	}
	total := len(data)
	if !bytes.HasSuffix(data, []byte("zv")) {
		return nil, errors.New("missing VZip footer")
	}

	propsByte := data[7]
	dictSize := int(binary.LittleEndian.Uint32(data[8:12]))
	if dictSize < 4096 {
		dictSize = 4096
	}

	decompressedSize := int64(int32(binary.LittleEndian.Uint32(data[total-6 : total-2])))
	if decompressedSize < 0 {
		return nil, errors.New("VZip negative decompressed size")
	}

	lzmaPayload := data[12 : total-10]

	// Classic .lzma format stream header (13 bytes):
	// props (1 byte) + dictSize (4 bytes) + uncompressedSize (8 bytes)
	lzmaHeader := make([]byte, 13)
	lzmaHeader[0] = propsByte
	copy(lzmaHeader[1:5], data[8:12])
	binary.LittleEndian.PutUint64(lzmaHeader[5:13], uint64(decompressedSize))

	fullStream := io.MultiReader(bytes.NewReader(lzmaHeader), bytes.NewReader(lzmaPayload))
	r, err := lzma.NewReader(fullStream)
	if err != nil {
		return nil, fmt.Errorf("lzma init error: %w", err)
	}

	out := make([]byte, decompressedSize)
	_, err = io.ReadFull(r, out)
	if err != nil {
		return nil, fmt.Errorf("lzma read error: %w", err)
	}
	return out, nil
}

func decompressZip(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip read error: %w", err)
	}
	if len(zr.File) != 1 {
		return nil, fmt.Errorf("zip archive contains %d entries, expected 1", len(zr.File))
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
