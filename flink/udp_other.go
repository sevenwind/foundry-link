//go:build !windows

package main

import "net"

func tuneUDP(c *net.UDPConn) {}
