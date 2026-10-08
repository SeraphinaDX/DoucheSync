// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Hashes are only cached in memory after a verified read in this process. A
// restart therefore verifies contents again; no on-disk history migration is
// needed. At most one cached entry exists per live, non-ignored path.
type cachedHash struct {
	entry    Entry
	info     os.FileInfo
	stamp    fileStamp
	verified time.Time
}

type fileStamp struct {
	first, second int64
	known         bool
}
type ScanStats struct {
	Full           bool
	Hashed, Reused int
	HashedBytes    int64
}

func sameFileStat(a, b os.FileInfo) bool {
	return a.Mode().IsRegular() && b.Mode().IsRegular() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode().Perm() == b.Mode().Perm() && os.SameFile(a, b)
}

func (f *Folder) changeStamp(p string, info os.FileInfo) (fileStamp, error) {
	if !stampNeedsHandle() {
		return fileChangeStamp(nil, info), nil
	}
	in, err := f.root.Open(filepath.FromSlash(p))
	if err != nil {
		return fileStamp{}, err
	}
	defer in.Close()
	current, err := in.Stat()
	if err != nil {
		return fileStamp{}, err
	}
	if !sameFileStat(info, current) {
		return fileStamp{}, errors.New("file changed while checking metadata")
	}
	return fileChangeStamp(in, current), nil
}

// Call with f.mu held. Unsupported change-time queries fall back to hashing;
// they never turn an unverifiable metadata match into a trusted cache hit.
func (f *Folder) cachedFileLocked(p string, info os.FileInfo, force bool) (Entry, bool, error) {
	if !info.Mode().IsRegular() {
		return Entry{}, false, errors.New("not a regular file")
	}
	if info.Size() > f.cfg.MaxFileSize {
		return Entry{}, false, fmt.Errorf("file exceeds max_file_size: %s", p)
	}
	if cached, ok := f.hashCache[p]; ok && !force && time.Since(cached.verified) < f.fullScanInterval && sameFileStat(cached.info, info) {
		stamp, err := f.changeStamp(p, info)
		if err != nil {
			return Entry{}, false, err
		}
		if stamp.known && stamp == cached.stamp {
			return cached.entry, true, nil
		}
	}
	e, current, stamp, err := f.hashFileVersion(context.Background(), p)
	if err != nil {
		return Entry{}, false, err
	}
	if f.hashCache == nil {
		f.hashCache = map[string]cachedHash{}
	}
	f.hashCache[p] = cachedHash{entry: e, info: current, stamp: stamp, verified: time.Now()}
	return e, false, nil
}
