package main

import (
    "archive/zip"
    "bytes"
    "encoding/binary"
    "errors"
    "fmt"
    "hash/crc32"
    "io"

    "github.com/klauspost/compress/zstd"
    "github.com/ulikunitz/xz/lzma"
)

var (
    magicLZMA = []byte{86, 90, 97}     // "VZa"
    magicZSTD = []byte{86, 83, 90, 97} // "VSZa"
    magicZIP  = []byte{80, 75, 3, 4}   // "PK\x03\x04"
)

func decompressInto(data []byte, out *[]byte) error {
    if len(data) < 4 {
        return errors.New("chunk too short")
    }
    *out = (*out)[:0]

    switch {
    case bytes.HasPrefix(data, magicLZMA):
        if len(data) < 22 {
            return errors.New("lzma chunk too short")
        }
        raw := data[12 : len(data)-9]
        size := binary.LittleEndian.Uint32(data[len(data)-6 : len(data)-2])
        crc := binary.LittleEndian.Uint32(data[len(data)-10 : len(data)-6])
        props := data[7]
        dictSize := binary.LittleEndian.Uint32(data[8:12])

        hdr := make([]byte, 13)
        hdr[0] = props
        binary.LittleEndian.PutUint32(hdr[1:5], dictSize)
        binary.LittleEndian.PutUint64(hdr[5:13], uint64(size))

        r, err := lzma.NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader(raw)))
        if err != nil {
            return fmt.Errorf("lzma init: %w", err)
        }
        dec := make([]byte, size)
        if _, err := io.ReadFull(r, dec); err != nil {
            return fmt.Errorf("lzma decode: %w", err)
        }
        if crc32.ChecksumIEEE(dec) != crc {
            return errors.New("lzma crc mismatch")
        }
        *out = dec
        return nil

    case bytes.HasPrefix(data, magicZSTD):
        if len(data) < 23 {
            return errors.New("zstd chunk too short")
        }
        raw := data[8 : len(data)-15]
        size := binary.LittleEndian.Uint32(data[len(data)-11 : len(data)-7])
        crc := binary.LittleEndian.Uint32(data[4:8])

        d, err := zstd.NewReader(nil)
        if err != nil {
            return err
        }
        defer d.Close()
        dec, err := d.DecodeAll(raw, nil)
        if err != nil {
            return fmt.Errorf("zstd decode: %w", err)
        }
        if uint32(len(dec)) < size {
            return errors.New("zstd short output")
        }
        dec = dec[:size]
        if crc32.ChecksumIEEE(dec) != crc {
            return errors.New("zstd crc mismatch")
        }
        *out = dec
        return nil

    case bytes.HasPrefix(data, magicZIP):
        zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
        if err != nil {
            return err
        }
        if len(zr.File) == 0 {
            return errors.New("zip empty")
        }
        f := zr.File[0]
        rc, err := f.Open()
        if err != nil {
            return err
        }
        defer rc.Close()
        buf, err := io.ReadAll(rc)
        if err != nil {
            return err
        }
        if crc32.ChecksumIEEE(buf) != f.CRC32 {
            return errors.New("zip crc mismatch")
        }
        *out = buf
        return nil
    }
    return errors.New("unknown chunk format")
}