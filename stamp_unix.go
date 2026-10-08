// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

//go:build linux || dragonfly || openbsd

package main

import (
	"os"
	"syscall"
)

func stampNeedsHandle() bool { return false }
func fileChangeStamp(_ *os.File, info os.FileInfo) fileStamp {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStamp{}
	}
	return fileStamp{first: int64(s.Ctim.Sec), second: int64(s.Ctim.Nsec), known: true}
}
