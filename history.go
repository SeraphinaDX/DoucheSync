// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const historyCheckpointInterval = 128
const maxUpdateBytes = 32 << 10

// Small atomic records retain exact version clocks if the process exits before
// a batch is checkpointed. state.json remains the existing version-1 format.
// This is local history, never advertised or sent to discovery.
type historyUpdate struct {
	Version int    `json:"version"`
	Marker  string `json:"marker"`
	Path    string `json:"path"`
	Entry   Entry  `json:"entry"`
}

func (f *Folder) updateNamesLocked() ([]string, error) {
	info, err := f.root.Lstat(".douchesync/updates")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("history updates must be a real directory")
	}
	d, err := f.root.Open(".douchesync/updates")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(maxEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > maxEntries {
		return nil, errors.New("too many pending history updates")
	}
	return names, nil
}

func (f *Folder) persistEntryLocked(p string, incremental bool) (err error) {
	if !incremental {
		return f.saveLocked()
	}
	if !f.healthy {
		return errors.New("folder history unavailable; retry after a successful scan")
	}
	defer func() {
		if err != nil {
			f.healthy = false
		}
	}()
	record := historyUpdate{Version: 1, Marker: f.state.Marker, Path: p, Entry: f.state.Entries[p]}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(b) > maxUpdateBytes {
		return errors.New("history update too large")
	}
	if _, err = f.updateNamesLocked(); err != nil {
		return err
	}
	name := ".douchesync/transfers/history-" + randomHex(16)
	out, err := f.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.root.Remove(name)
	_, err = out.Write(b)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = f.root.Rename(name, ".douchesync/updates/"+digest([]byte(p))+".json"); err != nil {
		return err
	}
	f.pendingUpdates++
	if f.pendingUpdates >= historyCheckpointInterval {
		return f.saveLocked()
	}
	return nil
}

func (f *Folder) replayUpdatesLocked() error {
	names, err := f.updateNamesLocked()
	if err != nil {
		return err
	}
	for _, name := range names {
		if len(name) != 69 || !strings.HasSuffix(name, ".json") {
			return errors.New("unrecognized history update filename")
		}
		p := ".douchesync/updates/" + name
		info, err := f.root.Lstat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxUpdateBytes {
			return errors.New("invalid history update file")
		}
		in, err := f.root.Open(p)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(in, maxUpdateBytes+1))
		in.Close()
		if err != nil {
			return err
		}
		if len(b) > maxUpdateBytes {
			return errors.New("history update too large")
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var record historyUpdate
		if err = dec.Decode(&record); err != nil {
			return err
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return errors.New("trailing history update data")
		}
		if record.Version != 1 || record.Marker != f.state.Marker || validatePath(record.Path) != nil || digest([]byte(record.Path))+".json" != name || validateEntry(record.Entry, 1<<50) != nil || record.Entry.Missing {
			return errors.New("invalid history update identity or entry")
		}
		old, exists := f.state.Entries[record.Path]
		if exists {
			switch compareClock(old.Clock, record.Entry.Clock) {
			case 1: // A later checkpoint already includes this update.
				f.pendingUpdates++
				continue
			case 0:
				if !sameContent(old, record.Entry) {
					return errors.New("inconsistent equal history clocks")
				}
				f.pendingUpdates++
				continue
			case 2:
				return errors.New("pending history conflicts with checkpoint")
			}
		} else if len(f.state.Entries) >= maxEntries {
			return errors.New("folder history exceeds 100000 paths")
		}
		f.state.Entries[record.Path] = record.Entry
		f.pendingUpdates++
	}
	return nil
}

func (f *Folder) discardUpdatesLocked() error {
	names, err := f.updateNamesLocked()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err = f.root.Remove(".douchesync/updates/" + name); err != nil {
			return fmt.Errorf("remove checkpointed history update: %w", err)
		}
	}
	f.pendingUpdates = 0
	return nil
}
