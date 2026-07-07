package transport

import (
	"io"
	"sync"
	"sync/atomic"
)

type closeWriter interface {
	CloseWrite() error
}

type closeOnly interface {
	Close() error
}

type Count struct {
	AToB uint64
	BToA uint64
}

type countWriter struct {
	w io.Writer
	n *atomic.Uint64
}

func (w countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n.Add(uint64(n))
	return n, err
}

func Pipe(a io.ReadWriteCloser, b io.ReadWriteCloser) Count {
	var aToB atomic.Uint64
	var bToA atomic.Uint64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(countWriter{w: b, n: &aToB}, a)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(countWriter{w: a, n: &bToA}, b)
		closeWrite(a)
	}()
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
	return Count{AToB: aToB.Load(), BToA: bToA.Load()}
}

func closeWrite(v any) {
	if cw, ok := v.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	if c, ok := v.(closeOnly); ok {
		_ = c.Close()
	}
}
