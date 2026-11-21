package manifest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// decodeMetadata parses ContentManifestMetadata from raw protobuf bytes.
func decodeMetadata(b []byte) (*ContentManifestMetadata, error) {
	meta := &ContentManifestMetadata{}
	r := &protoReader{buf: b}

	for r.hasMore() {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // depot_id uint32
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.DepotID = uint32(v)
		case 2: // gid_manifest uint64
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.GidManifest = v
		case 3: // creation_time uint32
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.CreationTime = uint32(v)
		case 4: // filenames_encrypted bool
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.FilenamesEncrypted = v != 0
		case 5: // cb_disk_original uint64
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.CbDiskOriginal = v
		case 6: // cb_disk_compressed uint64
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.CbDiskCompressed = v
		case 7: // unique_chunks uint32
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.UniqueChunks = uint32(v)
		case 8: // crc_encrypted uint32
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.CrcEncrypted = uint32(v)
		case 9: // crc_clear uint32
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			meta.CrcClear = uint32(v)
		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return meta, nil
}

// decodePayload parses ContentManifestPayload mappings from raw protobuf bytes.
func decodePayload(b []byte) ([]*FileMapping, error) {
	var files []*FileMapping
	r := &protoReader{buf: b}

	for r.hasMore() {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		if fieldNum == 1 && wireType == 2 { // mappings (repeated FileMapping)
			data, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			f, err := decodeFileMapping(data)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		} else {
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func decodeFileMapping(b []byte) (*FileMapping, error) {
	f := &FileMapping{}
	r := &protoReader{buf: b}

	for r.hasMore() {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // filename
			s, err := r.readString()
			if err != nil {
				return nil, err
			}
			f.Filename = s
		case 2: // size
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			f.Size = v
		case 3: // flags
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			f.Flags = uint32(v)
		case 4: // sha_filename
			data, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			f.ShaFilename = data
		case 5: // sha_content
			data, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			f.ShaContent = data
		case 6: // repeated ChunkData
			data, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			chunk, err := decodeChunkData(data)
			if err != nil {
				return nil, err
			}
			f.Chunks = append(f.Chunks, chunk)
		case 7: // linktarget
			s, err := r.readString()
			if err != nil {
				return nil, err
			}
			f.Linktarget = s
		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

func decodeChunkData(b []byte) (*ChunkData, error) {
	c := &ChunkData{}
	r := &protoReader{buf: b}

	for r.hasMore() {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch fieldNum {
		case 1: // sha (bytes)
			data, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			c.Sha = data
		case 2: // crc (fixed32)
			v, err := r.readFixed32()
			if err != nil {
				return nil, err
			}
			c.Crc = v
		case 3: // offset (uint64)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			c.Offset = v
		case 4: // cb_original (uint32)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			c.CbOriginal = uint32(v)
		case 5: // cb_compressed (uint32)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			c.CbCompressed = uint32(v)
		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

type protoReader struct {
	buf []byte
	pos int
}

func (p *protoReader) hasMore() bool {
	return p.pos < len(p.buf)
}

func (p *protoReader) readTag() (int, int, error) {
	v, err := p.readVarint()
	if err != nil {
		return 0, 0, err
	}
	fieldNum := int(v >> 3)
	wireType := int(v & 7)
	return fieldNum, wireType, nil
}

func (p *protoReader) readVarint() (uint64, error) {
	var val uint64
	var shift uint
	for {
		if p.pos >= len(p.buf) {
			return 0, io.ErrUnexpectedEOF
		}
		b := p.buf[p.pos]
		p.pos++
		val |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, errors.New("varint overflow")
		}
	}
	return val, nil
}

func (p *protoReader) readFixed32() (uint32, error) {
	if p.pos+4 > len(p.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	val := binary.LittleEndian.Uint32(p.buf[p.pos : p.pos+4])
	p.pos += 4
	return val, nil
}

func (p *protoReader) readFixed64() (uint64, error) {
	if p.pos+8 > len(p.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	val := binary.LittleEndian.Uint64(p.buf[p.pos : p.pos+8])
	p.pos += 8
	return val, nil
}

func (p *protoReader) readBytes() ([]byte, error) {
	length, err := p.readVarint()
	if err != nil {
		return nil, err
	}
	l := int(length)
	if p.pos+l > len(p.buf) {
		return nil, io.ErrUnexpectedEOF
	}
	data := p.buf[p.pos : p.pos+l]
	p.pos += l
	return data, nil
}

func (p *protoReader) readString() (string, error) {
	b, err := p.readBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *protoReader) skip(wireType int) error {
	switch wireType {
	case 0: // Varint
		_, err := p.readVarint()
		return err
	case 1: // 64-bit
		_, err := p.readFixed64()
		return err
	case 2: // Length-delimited
		_, err := p.readBytes()
		return err
	case 5: // 32-bit
		_, err := p.readFixed32()
		return err
	default:
		return fmt.Errorf("unsupported wire type: %d", wireType)
	}
}
