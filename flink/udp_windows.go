//go:build windows

package main

import (
	"net"
	"syscall"
	"unsafe"
)

// На Windows ICMP «порт недоступен» приходит как ошибка чтения и может оборвать приём.
// Отключаем это поведение (SIO_UDP_CONNRESET).
func tuneUDP(c *net.UDPConn) {
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		var ret uint32
		flag := uint32(0)
		_ = syscall.WSAIoctl(syscall.Handle(fd), 0x9800000C, (*byte)(unsafe.Pointer(&flag)), 4, nil, 0, &ret, nil, 0)
	})
}
