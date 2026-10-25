package detect

import (
	"strings"
	"syscall"
	"unsafe"
)

func running() map[string]bool {
	out := map[string]bool{}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		out[strings.ToLower(syscall.UTF16ToString(e.ExeFile[:]))] = true
	}
	return out
}
