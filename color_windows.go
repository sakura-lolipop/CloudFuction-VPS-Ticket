//go:build windows

// Windows 侧 VT 就绪（照抄 HotifyNEXT-Server color_windows.go）：经典 conhost 默认不解析 ANSI，
// 须 SetConsoleMode 打开 ENABLE_VIRTUAL_TERMINAL_PROCESSING（Windows Terminal/conpty 下再 or 一遍
// 无害）；失败 = 老 conhost，上色会显示 ←[32m 乱码 → 判不就绪。管道/文件 stdout GetConsoleMode
// 失败属正常：终端仿真交给接收端（mintty 解析 ANSI），判就绪。
package main

import (
	"os"
	"syscall"
	"unsafe"
)

const enableVirtualTerminalProcessing = 0x0004

// vtReady stdout 是 console 就尝试开 VT；非 console（管道/文件）直接就绪。
func vtReady() bool {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return true
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	handle := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if r, _, _ := getMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	r, _, _ := setMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
