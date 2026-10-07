// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Clock map[string]uint64
type Entry struct {
	Hash       string `json:"hash,omitempty"`
	Size       int64  `json:"size"`
	ModTime    int64  `json:"mod_time"`
	Mode       uint32 `json:"mode"`
	Deleted    bool   `json:"deleted,omitempty"`
	Missing    bool   `json:"missing,omitempty"` // local-only absence when deletion sync is disabled
	Conflicted bool   `json:"conflicted,omitempty"`
	Clock      Clock  `json:"clock"`
}
type State struct {
	Version int              `json:"version"`
	Device  string           `json:"device"`
	Room    string           `json:"room"`
	Marker  string           `json:"marker"`
	Entries map[string]Entry `json:"entries"`
}
type Folder struct {
	cfg            FolderConfig
	device         string
	root           *os.Root
	lock           *os.File
	mu             sync.Mutex
	state          State
	healthy        bool
	pendingUpdates int
}

func cloneClock(c Clock) Clock {
	out := Clock{}
	for k, v := range c {
		out[k] = v
	}
	return out
}
func joinClock(a, b Clock) Clock {
	out := cloneClock(a)
	for k, v := range b {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

// compareClock returns 1 if a dominates, -1 if b dominates, 0 if equal,
// and 2 if both contain independent edits. It never uses wall-clock ordering.
func compareClock(a, b Clock) int {
	aa, bb := false, false
	for k, v := range a {
		if v > b[k] {
			aa = true
		}
	}
	for k, v := range b {
		if v > a[k] {
			bb = true
		}
	}
	if aa && bb {
		return 2
	}
	if aa {
		return 1
	}
	if bb {
		return -1
	}
	return 0
}
func sameContent(a, b Entry) bool {
	return a.Deleted == b.Deleted && (a.Deleted || (a.Hash == b.Hash && a.Size == b.Size))
}
func winner(a, b Entry) Entry {
	// An edit concurrent with a delete survives. Two edits use hash order;
	// every peer chooses the same canonical file and retains the other copy.
	if a.Deleted != b.Deleted {
		if a.Deleted {
			return b
		}
		return a
	}
	if a.Hash >= b.Hash {
		return a
	}
	return b
}
func validatePath(name string) error {
	if name == "" || len(name) > 4096 || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return errors.New("invalid relative path")
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return errors.New("control character in filename")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".douchesync") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsAny(part, `<>"|?*`) {
			return errors.New("reserved or nonportable filename")
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return errors.New("reserved Windows filename")
		}
	}
	return nil
}
func validateEntry(e Entry, limit int64) error {
	if len(e.Clock) == 0 || len(e.Clock) > 128 {
		return errors.New("invalid version clock")
	}
	for k, v := range e.Clock {
		if !validID.MatchString(k) || v == 0 || v == ^uint64(0) {
			return errors.New("invalid clock counter/device")
		}
	}
	if e.Size < 0 || e.Size > limit || e.Mode > 0777 {
		return errors.New("invalid size or mode")
	}
	if !e.Deleted {
		if len(e.Hash) != 64 {
			return errors.New("invalid content hash")
		}
		if _, err := hex.DecodeString(e.Hash); err != nil {
			return err
		}
	}
	return nil
}
func (f *Folder) ignored(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".douchesync") {
			return true
		}
	}
	// A directory pattern also excludes every descendant when receiving files.
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		for _, pattern := range f.cfg.Ignore {
			if ok, _ := filepath.Match(pattern, filepath.FromSlash(prefix)); ok {
				return true
			}
			if ok, _ := filepath.Match(pattern, path.Base(prefix)); ok {
				return true
			}
		}
	}
	return false
}
func (f *Folder) checkAncestors(name string) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		prefix := filepath.FromSlash(strings.Join(parts[:i+1], "/"))
		info, err := f.root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not synchronized")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return errors.New("parent path is not a directory")
		}
	}
	return nil
}
func openFolder(cfg FolderConfig, device string) (*Folder, error) {
	r, err := os.OpenRoot(cfg.Path)
	if err != nil {
		return nil, err
	}
	f := &Folder{cfg: cfg, device: device, root: r}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	if err = r.MkdirAll(".douchesync", 0700); err != nil {
		return nil, err
	}
	// Refuse symlinked internal storage, even when its target is inside the root.
	if info, err := r.Lstat(".douchesync"); err != nil || !info.IsDir() {
		return nil, errors.New(".douchesync must be a real directory")
	}
	f.lock, err = r.OpenFile(".douchesync/lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(f.lock); err != nil {
		return nil, fmt.Errorf("folder already in use: %w", err)
	}
	f.state = State{Version: 1, Device: device, Room: roomID(cfg), Marker: randomHex(16), Entries: map[string]Entry{}}
	stateFile, err := r.Open(".douchesync/state.json")
	if err == nil {
		dec := json.NewDecoder(io.LimitReader(stateFile, maxManifestBytes+1))
		dec.DisallowUnknownFields()
		err = dec.Decode(&f.state)
		stateFile.Close()
		if err != nil {
			return nil, fmt.Errorf("state file: %w", err)
		}
		if f.state.Version != 1 || f.state.Device != device || f.state.Room != roomID(cfg) || f.state.Marker == "" || f.state.Entries == nil || len(f.state.Entries) > maxEntries {
			return nil, errors.New("state identity/version mismatch; do not share .douchesync or change device IDs/secrets without resetting history")
		}
		for p, e := range f.state.Entries {
			if validatePath(p) != nil || validateEntry(e, 1<<50) != nil {
				return nil, errors.New("invalid saved state")
			}
		}
		marker, e := r.ReadFile(".douchesync/identity")
		if e != nil || string(marker) != f.state.Marker {
			return nil, errors.New("folder identity marker missing or changed")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if _, e := r.Lstat(".douchesync/identity"); !errors.Is(e, os.ErrNotExist) {
			return nil, errors.New("state is missing from an initialized folder; restore state or explicitly reset .douchesync")
		}
		if err = r.WriteFile(".douchesync/identity", []byte(f.state.Marker), 0600); err != nil {
			return nil, err
		}
		if err = f.saveLocked(); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err = r.MkdirAll(".douchesync/transfers", 0700); err != nil {
		return nil, err
	}
	f.healthy = true
	if err = r.MkdirAll(".douchesync/conflicts", 0700); err != nil {
		return nil, err
	}
	if err = r.MkdirAll(".douchesync/versions", 0700); err != nil {
		return nil, err
	}
	if err = r.MkdirAll(".douchesync/updates", 0700); err != nil {
		return nil, err
	}
	if err = f.replayUpdatesLocked(); err != nil {
		return nil, fmt.Errorf("pending history updates: %w", err)
	}
	if f.pendingUpdates != 0 {
		if err = f.saveLocked(); err != nil {
			return nil, err
		}
	}
	// A previous crash may leave an incomplete transfer, never a canonical file.
	d, err := r.Open(".douchesync/transfers")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if err = r.Remove(".douchesync/transfers/" + n); err != nil {
			return nil, err
		}
	}
	ok = true
	return f, nil
}
func (f *Folder) Close() {
	if f.lock != nil {
		f.lock.Close()
	}
	if f.root != nil {
		f.root.Close()
	}
}
func (f *Folder) saveLocked() (err error) {
	defer func() { f.healthy = err == nil }()
	b, err := json.MarshalIndent(f.state, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxManifestBytes {
		return errors.New("state exceeds 64 MiB limit")
	}
	name := ".douchesync/state-" + randomHex(8) + ".tmp"
	out, err := f.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.root.Remove(name)
	_, err = out.Write(b)
	if err == nil {
		err = out.Sync()
	}
	ce := out.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = f.root.Rename(name, ".douchesync/state.json"); err != nil {
		return err
	}
	// If a crash occurs after the checkpoint rename but before cleanup, replay
	// skips records whose clocks are already included in the checkpoint.
	return f.discardUpdatesLocked()
}
func (f *Folder) snapshot() map[string]Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotLocked()
}
func (f *Folder) manifest() (map[string]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.healthy {
		return nil, errors.New("folder state could not be saved")
	}
	return f.snapshotLocked(), nil
}
func (f *Folder) snapshotLocked() map[string]Entry {
	out := map[string]Entry{}
	for p, e := range f.state.Entries {
		if e.Missing {
			continue
		}
		e.Clock = cloneClock(e.Clock)
		out[p] = e
	}
	return out
}

// hashFile checks for changes during hashing. An unstable file is retried next
// cycle; a scan failure never turns unreadable files into deletion records.
func (f *Folder) hashFile(name string) (Entry, error) {
	e, _, err := f.hashFileObserved(context.Background(), name)
	return e, err
}

// The returned file identity permits a cheap final check after hashing outside
// the folder lock. Full hashes still detect content edits, even with preserved
// timestamps; the final identity/stat check catches replacement while waiting
// to commit.
func (f *Folder) hashFileObserved(ctx context.Context, name string) (Entry, os.FileInfo, error) {
	in, err := f.root.Open(filepath.FromSlash(name))
	if err != nil {
		return Entry{}, nil, err
	}
	defer in.Close()
	before, err := in.Stat()
	if err != nil {
		return Entry{}, nil, err
	}
	if !before.Mode().IsRegular() {
		return Entry{}, nil, errors.New("not a regular file")
	}
	if before.Size() > f.cfg.MaxFileSize {
		return Entry{}, nil, fmt.Errorf("file exceeds max_file_size: %s", name)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, in: in}, f.cfg.MaxFileSize+1))
	if err != nil {
		return Entry{}, nil, err
	}
	after, err := in.Stat()
	if err != nil {
		return Entry{}, nil, err
	}
	current, err := f.root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return Entry{}, nil, err
	}
	if !os.SameFile(before, current) || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || current.Size() != after.Size() || !current.ModTime().Equal(after.ModTime()) || current.Mode().Perm() != after.Mode().Perm() {
		return Entry{}, nil, fmt.Errorf("file changed during scan: %s", name)
	}
	return Entry{Hash: hex.EncodeToString(h.Sum(nil)), Size: n, ModTime: after.ModTime().UnixNano(), Mode: uint32(after.Mode().Perm()), Clock: Clock{}}, current, nil
}
func (f *Folder) observeLocked(p string, live *Entry) error {
	old, ok := f.state.Entries[p]
	if live == nil {
		if !ok || old.Deleted {
			return nil
		}
		if !f.cfg.SyncDeletes {
			old.Missing = true
			f.state.Entries[p] = old
			return nil
		}
		old.Clock = cloneClock(old.Clock)
		if old.Clock[f.device] == 0 && len(old.Clock) >= 128 {
			return errors.New("version history exceeds 128 devices")
		}
		if old.Clock[f.device] >= ^uint64(0)-1 {
			return errors.New("version counter exhausted")
		}
		old.Clock[f.device]++
		old.Deleted = true
		old.Missing = false
		old.Hash = ""
		old.Size = 0
		f.state.Entries[p] = old
		return nil
	}
	next := *live
	if ok && sameContent(old, next) {
		next.Clock = cloneClock(old.Clock)
		next.Conflicted = old.Conflicted
	} else {
		if ok {
			next.Clock = cloneClock(old.Clock)
		} else {
			next.Clock = Clock{}
		}
		if next.Clock[f.device] == 0 && len(next.Clock) >= 128 {
			return errors.New("version history exceeds 128 devices")
		}
		if next.Clock[f.device] >= ^uint64(0)-1 {
			return errors.New("version counter exhausted")
		}
		next.Clock[f.device]++
	}
	f.state.Entries[p] = next
	return nil
}
func (f *Folder) Scan() (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() {
		if err != nil {
			f.healthy = false
		}
	}()
	marker, err := f.root.ReadFile(".douchesync/identity")
	if err != nil || string(marker) != f.state.Marker {
		return errors.New("folder disappeared/identity changed; refusing scan")
	}
	seen := map[string]Entry{}
	err = fs.WalkDir(f.root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == "." {
			return nil
		}
		if f.ignored(p) || validatePath(p) != nil {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if len(seen) >= maxEntries {
			return errors.New("folder exceeds 100000 files")
		}
		entry, e := f.hashFile(p)
		if e != nil {
			return e
		}
		seen[p] = entry
		return nil
	})
	if err != nil {
		return err
	}
	for p, e := range seen {
		if _, exists := f.state.Entries[p]; !exists && len(f.state.Entries) >= maxEntries {
			return errors.New("folder history exceeds 100000 paths; no new files accepted")
		}
		if err = f.observeLocked(p, &e); err != nil {
			return err
		}
	}
	for p := range f.state.Entries {
		if f.ignored(p) {
			delete(f.state.Entries, p)
			continue
		}
		if _, ok := seen[p]; !ok {
			// If a tracked path has become a symlink/device/directory, stop rather
			// than mistaking that obstruction for a deletion.
			if _, e := f.root.Lstat(filepath.FromSlash(p)); e == nil {
				return fmt.Errorf("tracked path is no longer a regular file: %s", p)
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
			if err = f.observeLocked(p, nil); err != nil {
				return err
			}
		}
	}
	return f.saveLocked()
}
func (f *Folder) refreshLocked(p string) error {
	if err := f.checkAncestors(p); err != nil {
		return err
	}
	info, err := f.root.Lstat(filepath.FromSlash(p))
	old, existed := f.state.Entries[p]
	if errors.Is(err, os.ErrNotExist) {
		if err = f.observeLocked(p, nil); err != nil {
			return err
		}
		return f.persistObservationLocked(p, old, existed)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("destination is not a regular file")
	}
	e, err := f.hashFile(p)
	if err != nil {
		return err
	}
	if err = f.observeLocked(p, &e); err != nil {
		return err
	}
	return f.persistObservationLocked(p, old, existed)
}
func (f *Folder) persistObservationLocked(p string, old Entry, existed bool) error {
	now, present := f.state.Entries[p]
	if existed != present || (existed && (!sameContent(old, now) || old.Missing != now.Missing || compareClock(old.Clock, now.Clock) != 0)) {
		return f.saveLocked()
	}
	return nil
}
func sortedPaths(m map[string]Entry) []string {
	keys := make([]string, 0, len(m))
	for p := range m {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	return keys
}

// archiveLocal retains a byte-verified version before replacement.
// Archive keys include the original pathname hash and content hash, preventing
// collisions and keeping overly long original names out of the archive paths.
func archiveName(category, p string, e Entry) string {
	return ".douchesync/" + category + "/" + digest([]byte(p)) + "-" + e.Hash
}
func (f *Folder) archiveLocal(p string, e Entry, category string) error {
	if e.Deleted {
		return nil
	}
	name := archiveName(category, p, e)
	in, err := f.root.Open(filepath.FromSlash(p))
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := ".douchesync/transfers/archive-" + randomHex(16)
	out, err := f.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.root.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, e.Size+1))
	if err == nil {
		err = out.Sync()
	}
	ce := out.Close()
	if err == nil {
		err = ce
	}
	if err != nil || n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.Hash {
		_ = f.root.Remove(tmp)
		if err != nil {
			return err
		}
		return errors.New("local file changed while saving version")
	}
	if err = f.root.Rename(tmp, name); err != nil {
		return err
	}
	// Catch a save performed through an atomic rename while the old inode was
	// being archived. Preserve that new local edit for the next sync cycle.
	latest, err := f.hashFile(p)
	if err != nil {
		return err
	}
	if !sameContent(latest, e) {
		return errors.New("local file changed while saving version")
	}
	meta, _ := json.MarshalIndent(map[string]any{"path": p, "entry": e}, "", "  ")
	if err = f.root.WriteFile(name+".json", meta, 0600); err != nil {
		return err
	}
	if category == "conflicts" {
		log.Printf("[%s] conflict saved: %s", f.cfg.ID, p)
	}
	return nil
}
