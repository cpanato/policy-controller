// Copyright 2022 The Sigstore Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tuf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/theupdateframework/go-tuf/client"
)

func CompressFS(fsys fs.FS, buf io.Writer, skipDirs map[string]bool) error {
	// tar > gzip > buf
	zr := gzip.NewWriter(buf)
	tw := tar.NewWriter(zr)

	walkErr := fs.WalkDir(fsys, "repository", func(file string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// If we encounter an error walking, just return it and give up.
			return walkErr
		}
		// Skip the 'keys' and 'staged' directory
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}

		// Stat the file to get the details of it.
		fi, err := fs.Stat(fsys, file)
		if err != nil {
			return fmt.Errorf("fs.Stat %s: %w", file, err)
		}
		header, err := tar.FileInfoHeader(fi, file)
		if err != nil {
			return fmt.Errorf("FileInfoHeader %s: %w", file, err)
		}
		header.Name = filepath.ToSlash(file)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		// For files, write the contents.
		if !d.IsDir() {
			data, err := fsys.Open(file)
			if err != nil {
				return fmt.Errorf("opening %s: %w", file, err)
			}
			if _, err := io.Copy(tw, data); err != nil {
				return fmt.Errorf("copying %s: %w", file, err)
			}
		}
		return nil
	})

	if walkErr != nil {
		tw.Close()
		zr.Close()
		return fmt.Errorf("WalkDir: %w", walkErr)
	}

	if err := tw.Close(); err != nil {
		zr.Close()
		return fmt.Errorf("tar.NewWriter Close(): %w", err)
	}
	return zr.Close()
}

func Uncompress(src io.Reader, dst string) error {
	zr, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)

	// uncompress each element
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break // End of archive
		}
		if err != nil {
			return err
		}

		target, err := sanitizeArchivePath(dst, header.Name)
		// validate name against path traversal
		if err != nil {
			return err
		}

		// check the type
		switch header.Typeflag {
		// Create directories
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, os.ModePerm); err != nil {
					return err
				}
			}
		// Write out files
		case tar.TypeReg:
			fileToWrite, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			// copy over contents in chunks for security reasons
			// G110: Potential DoS vulnerability via decompression bomb
			for {
				_, err := io.CopyN(fileToWrite, tr, 1024)
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return err
				}
			}

			if err := fileToWrite.Close(); err != nil {
				return fmt.Errorf("failed to close file %s: %w", target, err)
			}
		}
	}
	return nil
}

// From https://github.com/securego/gosec/issues/324
func sanitizeArchivePath(d, t string) (v string, err error) {
	v = filepath.Join(d, t)
	if strings.HasPrefix(v, filepath.Clean(d)) {
		return v, nil
	}

	return "", fmt.Errorf("%s: %s", "content filepath is tainted", t)
}

// UncompressMemFS takes a TUF repository that's been compressed with CompressFS
// and returns FS backed by memory.
func UncompressMemFS(src io.Reader) (fs.FS, error) {
	testFS := fstest.MapFS{}

	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)

	// uncompress each element
	for {
		header, err := tr.Next()
		// EOF is unwrapped
		//nolint:errorlint
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return nil, err
		}
		target, err := sanitizeArchivePath("/", header.Name)
		// validate name against path traversal
		if err != nil {
			return nil, err
		}

		// check the type
		if header.Typeflag == tar.TypeReg {
			data := make([]byte, header.Size)
			_, err := tr.Read(data)
			// EOF is unwrapped
			//nolint:errorlint
			if err != nil && err != io.EOF {
				return nil, fmt.Errorf("reading file %s : %w", header.Name, err)
			}
			testFS[target] = &fstest.MapFile{
				Data:    data,
				Mode:    os.FileMode(header.Mode),
				ModTime: header.ModTime,
			}
		}
	}
	return testFS, nil
}

// ClientFromSerializedMirror will construct a TUF client by
// unserializing the repository and constructing an in-memory client
// for it.
func ClientFromSerializedMirror(ctx context.Context, repo []byte, targets string) (*client.Client, error) {
	r, err := base64.StdEncoding.DecodeString(string(repo))
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode: %w", err)
	}
	// unzip/untar the repository.
	tufFS, err := UncompressMemFS(bytes.NewReader(r))
	if err != nil {
		return nil, fmt.Errorf("failed to uncompress: %w", err)
	}
	remote, err := client.NewFileRemoteStore(tufFS, targets)
	if err != nil {
		return nil, fmt.Errorf("failed to create remote store: %w", err)
	}

	local := client.MemoryLocalStore()
	tufClient := client.NewClient(local, remote)

	// TODO(vaikas): What should we do with above tufClient validation
	// wise before just returning it?
	return tufClient, err
}