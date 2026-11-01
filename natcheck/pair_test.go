package main

import (
	"net"
	"testing"
	"time"
)

func TestCodeRoundTrip(t *testing.T) {
	in := pairCode{role: roleHost, nat: natCone, sid: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		ep: &net.UDPAddr{IP: net.IPv4(188, 243, 223, 240), Port: 51811}}
	s := encodeCode(in)
	out, err := decodeCode(" " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.role != in.role || out.nat != in.nat || out.sid != in.sid || out.ep.String() != in.ep.String() {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
	// опечатка должна ловиться
	bad := []byte(s)
	bad[8] = 'A'
	if bad[8] == s[8] {
		bad[8] = 'B'
	}
	if _, err := decodeCode(string(bad)); err == nil {
		t.Fatal("опечатка не обнаружена")
	}
}

func TestPunchAndMeasureLoopback(t *testing.T) {
	plan.quick, plan.total = 5, 8
	plan.fast, plan.slow = 20*time.Millisecond, 40*time.Millisecond
	plan.tail, plan.linger = 200*time.Millisecond, 300*time.Millisecond

	lo := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	a, _ := net.ListenUDP("udp4", lo)
	b, _ := net.ListenUDP("udp4", lo)
	defer a.Close()
	defer b.Close()
	aAddr, bAddr := a.LocalAddr().(*net.UDPAddr), b.LocalAddr().(*net.UDPAddr)
	sid := [8]byte{9, 9, 9}

	type res struct {
		p punchResult
		m measureResult
	}
	run := func(c *net.UDPConn, peer *net.UDPAddr, ch chan<- res) {
		p := punch(c, peer, sid, 5*time.Second, nil)
		var m measureResult
		if p.established {
			m = measure(c, p.observed, sid)
		}
		ch <- res{p, m}
	}
	ca, cb := make(chan res, 1), make(chan res, 1)
	go run(a, bAddr, ca)
	go run(b, aAddr, cb)
	for _, ch := range []chan res{ca, cb} {
		r := <-ch
		if !r.p.established {
			t.Fatal("не установилось")
		}
		if r.m.sent != 8 || r.m.recv != 8 {
			t.Fatalf("sent=%d recv=%d", r.m.sent, r.m.recv)
		}
	}
}
