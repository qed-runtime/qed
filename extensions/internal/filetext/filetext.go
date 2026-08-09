// Package filetext reads bounded UTF-8 files for workspace-scoped Extensions
package filetext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// Document contains one bounded UTF-8 file and its preimage identity
type Document struct {
	Data   []byte
	Digest string
	Mode   os.FileMode
}

// Read loads one open regular UTF-8 text file no larger than maxBytes
func Read(file *os.File, maxBytes int64) (Document, error) {
	if file == nil {
		return Document{}, errors.New("file must not be nil")
	}
	if maxBytes <= 0 {
		return Document{}, errors.New("maximum file size must be positive")
	}
	info, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	if !info.Mode().IsRegular() {
		return Document{}, errors.New("path must be a regular file")
	}
	if info.Size() > maxBytes {
		return Document{}, fmt.Errorf("file exceeds %d bytes", maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return Document{}, err
	}
	if int64(len(data)) > maxBytes {
		return Document{}, fmt.Errorf("file exceeds %d bytes", maxBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return Document{}, errors.New("binary file is not supported")
	}
	if !utf8.Valid(data) {
		return Document{}, errors.New("file must contain valid UTF-8")
	}
	sum := sha256.Sum256(data)
	return Document{
		Data:   data,
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Mode:   info.Mode().Perm(),
	}, nil
}
