// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// PeerIdentity is generated automatically, then reused across restarts. The
// private key belongs to this client and never goes to discovery or peers.
type PeerIdentity struct {
	Certificate tls.Certificate
	Fingerprint string
	lock        *os.File
}

func (i *PeerIdentity) Close() {
	if i != nil && i.lock != nil {
		_ = i.lock.Close()
	}
}

func identityPath(cfg ClientConfig) (string, error) {
	if cfg.IdentityDir != "" {
		return expandPath(cfg.IdentityDir)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "douchesync", "identity"), nil
}

func openPeerIdentity(cfg ClientConfig) (*PeerIdentity, error) {
	if !validID.MatchString(cfg.DeviceID) {
		return nil, errors.New("invalid device ID for peer identity")
	}
	dir, err := identityPath(cfg)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create identity directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	identity := &PeerIdentity{}
	ok := false
	defer func() {
		if !ok {
			identity.Close()
		}
	}()
	identity.lock, err = root.OpenFile(cfg.DeviceID+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(identity.lock); err != nil {
		return nil, fmt.Errorf("device %s is already running with this identity: %w", cfg.DeviceID, err)
	}
	name := cfg.DeviceID + ".pem"
	info, err := root.Lstat(name)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("peer identity must be a regular file")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("peer identity contains a private key; set permissions to 0600 on %s", filepath.Join(dir, name))
		}
		if info.Size() > 64<<10 {
			return nil, errors.New("peer identity file exceeds 64 KiB")
		}
		in, e := root.Open(name)
		if e != nil {
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(in, (64<<10)+1))
		_ = in.Close()
		if e != nil {
			return nil, e
		}
		if len(data) > 64<<10 {
			return nil, errors.New("peer identity file exceeds 64 KiB")
		}
		identity.Certificate, err = tls.X509KeyPair(data, data)
		if err != nil {
			return nil, fmt.Errorf("cannot load saved peer identity %s (will not silently replace it): %w", filepath.Join(dir, name), err)
		}
		identity.Fingerprint = digest(identity.Certificate.Certificate[0])
	} else if errors.Is(err, os.ErrNotExist) {
		identity.Certificate, identity.Fingerprint, err = peerCertificate()
		if err != nil {
			return nil, err
		}
		key, e := x509.MarshalPKCS8PrivateKey(identity.Certificate.PrivateKey)
		if e != nil {
			return nil, e
		}
		data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.Certificate.Certificate[0]})
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
		tmp := "." + cfg.DeviceID + "-" + randomHex(16) + ".tmp"
		out, e := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		defer root.Remove(tmp)
		_, e = out.Write(data)
		if e == nil {
			e = out.Sync()
		}
		closeErr := out.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return nil, e
		}
		if err = root.Rename(tmp, name); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	ok = true
	return identity, nil
}
