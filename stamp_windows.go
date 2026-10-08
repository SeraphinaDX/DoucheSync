// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var getFileBasicInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")

func stampNeedsHandle() bool { return true }
func fileChangeStamp(in *os.File, _ os.FileInfo) fileStamp {
	if in == nil || getFileBasicInfo.Find() != nil {
		return fileStamp{}
	}
	// FILE_BASIC_INFO, FileBasicInfo=0. ChangeTime detects metadata/content
	// changes even when an application restores the last-write timestamp.
	// https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_basic_info
	var basic struct {
		creation, access, write, change int64
		attributes, padding             uint32
	}
	r, _, _ := getFileBasicInfo.Call(in.Fd(), 0, uintptr(unsafe.Pointer(&basic)), unsafe.Sizeof(basic))
	runtime.KeepAlive(in)
	if r == 0 || basic.change == 0 {
		return fileStamp{}
	}
	return fileStamp{first: basic.change, known: true}
}
