package main

import (
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

var bufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

// copyCount копирует данные и считает байты. EOF считается успехом.
func copyCount(dst io.Writer, src io.Reader, ctr *atomic.Int64) error {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			ctr.Add(int64(w))
			if werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// pipe соединяет TCP-соединение и QUIC-поток в обе стороны с корректным полузакрытием.
// sent — байты tcp→quic, recv — байты quic→tcp.
func pipe(tcp *net.TCPConn, st quic.Stream, sent, recv *atomic.Int64) {
	_ = tcp.SetNoDelay(true)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := copyCount(st, tcp, sent); err != nil {
			st.CancelWrite(1)
			_ = tcp.CloseRead()
			return
		}
		_ = st.Close()
	}()
	go func() {
		defer wg.Done()
		if err := copyCount(tcp, st, recv); err != nil {
			st.CancelRead(1)
			_ = tcp.CloseWrite()
			return
		}
		_ = tcp.CloseWrite()
	}()
	wg.Wait()
	_ = tcp.Close()
}
