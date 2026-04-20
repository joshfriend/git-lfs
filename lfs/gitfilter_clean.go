package lfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"github.com/git-lfs/git-lfs/v3/errors"
	"github.com/git-lfs/git-lfs/v3/tools"
	"github.com/rubyist/tracerx"
)

type cleanedAsset struct {
	Filename string
	*Pointer
}

func (f *GitFilter) Clean(reader io.Reader, fileName string, fileSize int64, cb tools.CopyCallback) (*cleanedAsset, error) {
	extensions, err := f.cfg.SortedExtensions()
	if err != nil {
		return nil, err
	}

	var oid string
	var size int64
	var tmp *os.File
	var exts []*PointerExtension
	if len(extensions) > 0 {
		request := &pipeRequest{"clean", reader, fileName, extensions}

		var response pipeResponse
		if response, err = pipeExtensions(f.cfg, request); err != nil {
			return nil, err
		}

		oid = response.results[len(response.results)-1].oidOut
		tmp = response.file
		var stat os.FileInfo
		if stat, err = os.Stat(tmp.Name()); err != nil {
			return nil, err
		}
		size = stat.Size()

		for _, result := range response.results {
			if result.oidIn != result.oidOut {
				ext := NewPointerExtension(result.name, len(exts), result.oidIn)
				exts = append(exts, ext)
			}
		}
	} else {
		oid, size, tmp, err = f.copyToTemp(reader, fileName, fileSize, cb)
		if err != nil {
			return nil, err
		}
	}

	pointer := NewPointer(oid, size, exts)
	tmpName := ""
	if tmp != nil {
		tmpName = tmp.Name()
	}
	return &cleanedAsset{tmpName, pointer}, err
}

// copyToTemp hashes the content from reader and optionally writes it to a temp
// file. If the LFS object for the computed OID already exists in the local
// store, no temp file is created (tmp will be nil), avoiding unnecessary disk
// I/O. This is a significant optimization for operations like git diff that
// invoke the clean filter on files whose objects are already stored locally.
//
// If the object does not exist, the content is re-read from fileName (the
// working tree path) and written to a temp file for later storage. If fileName
// is not available, the content is written to a temp file during the initial
// hash pass as a fallback.
func (f *GitFilter) copyToTemp(reader io.Reader, fileName string, fileSize int64, cb tools.CopyCallback) (oid string, size int64, tmp *os.File, err error) {
	if fileSize <= 0 {
		cb = nil
	}

	// Check if the content is already a pointer (small enough to be one).
	ptr, buf, perr := DecodeFrom(reader)

	by := make([]byte, blobSizeCutoff)
	n, rerr := buf.Read(by)
	by = by[:n]

	if rerr != nil || (perr == nil && len(by) < blobSizeCutoff) {
		err = errors.NewCleanPointerError(ptr, by)
		return
	}

	var from io.Reader = bytes.NewReader(by)
	if fileSize < 0 || int64(len(by)) < fileSize {
		from = io.MultiReader(from, reader)
	}

	// If we have a working tree path we can re-read from, hash the pipe
	// content without writing a temp file. If the object already exists,
	// we skip all temp file I/O entirely.
	_, statErr := os.Stat(fileName)
	canReread := len(fileName) > 0 && statErr == nil
	if canReread {
		oid, size, err = f.hashOnly(from, fileSize, cb)
		if err != nil {
			return
		}

		if f.objectExists(oid, size) {
			tracerx.Printf("clean: object %s already exists, skipping temp file", oid)
			return
		}

		// Object doesn't exist; re-read from the working tree file to
		// create the temp file for later storage.
		tracerx.Printf("clean: object %s not found, re-reading from %s", oid, fileName)
		tmp, err = f.writeWorkingTreeToTemp(fileName)
		return
	}

	// Fallback: no working tree path available, tee to temp file while hashing.
	tmp, err = TempFile(f.cfg, "")
	if err != nil {
		return
	}
	defer tmp.Close()

	oidHash := sha256.New()
	writer := io.MultiWriter(oidHash, tmp)
	size, err = tools.CopyWithCallback(writer, from, fileSize, cb)
	if err != nil {
		return
	}
	oid = hex.EncodeToString(oidHash.Sum(nil))
	return
}

// hashOnly reads all content from r, computes the SHA-256 hash, and returns
// the OID and total size without writing anything to disk.
func (f *GitFilter) hashOnly(r io.Reader, fileSize int64, cb tools.CopyCallback) (oid string, size int64, err error) {
	oidHash := sha256.New()
	size, err = tools.CopyWithCallback(oidHash, r, fileSize, cb)
	if err != nil {
		return
	}
	oid = hex.EncodeToString(oidHash.Sum(nil))
	return
}

// objectExists checks whether an LFS object with the given OID and size
// already exists in the local object store.
func (f *GitFilter) objectExists(oid string, size int64) bool {
	return f.cfg.LFSObjectExists(oid, size)
}

// writeWorkingTreeToTemp reads a file from the working tree and writes it
// to a temp file suitable for moving into the LFS object store.
func (f *GitFilter) writeWorkingTreeToTemp(fileName string) (*os.File, error) {
	src, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	tmp, err := TempFile(f.cfg, "")
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}

	tmp.Close()
	return tmp, nil
}

func (a *cleanedAsset) Teardown() error {
	if a.Filename == "" {
		return nil
	}
	return os.Remove(a.Filename)
}
