// Copyright 2016 The CMux Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package cmux

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Matcher matches a connection based on its content.
type Matcher func(io.Reader) bool

// MatchWriter is a match that can also write response (say to do handshake).
type MatchWriter func(io.Writer, io.Reader) bool

// ErrorHandler handles an error and returns whether
// the mux should continue serving the listener.
type ErrorHandler func(error) bool

var _ net.Error = ErrNotMatched{}

// ErrNotMatched is returned whenever a connection is not matched by any of
// the matchers registered in the multiplexer.
type ErrNotMatched struct {
	c net.Conn
}

func (e ErrNotMatched) Error() string {
	return fmt.Sprintf("mux: connection %v not matched by an matcher",
		e.c.RemoteAddr())
}

// Temporary implements the net.Error interface.
func (e ErrNotMatched) Temporary() bool { return true }

// Timeout implements the net.Error interface.
func (e ErrNotMatched) Timeout() bool { return false }

type errListenerClosed string

func (e errListenerClosed) Error() string   { return string(e) }
func (e errListenerClosed) Temporary() bool { return false }
func (e errListenerClosed) Timeout() bool   { return false }

// ErrListenerClosed is returned from muxListener.Accept when the underlying
// listener is closed.
var ErrListenerClosed = errListenerClosed("mux: listener closed")

// for readability of readTimeout
var noTimeout time.Duration

// New instantiates a new connection multiplexer.
func New(l net.Listener) CMux {
	return &cMux{
		root:        l,
		bufLen:      1024,
		errh:        func(_ error) bool { return true },
		donec:       make(chan struct{}),
		readTimeout: noTimeout,
	}
}

// CMux is a multiplexer for network connections.
type CMux interface {
	// Match returns a net.Listener that sees (i.e., accepts) only
	// the connections matched by at least one of the matcher.
	//
	// The order used to call Match determines the priority of matchers.
	Match(...Matcher) net.Listener
	// MatchWithWriters returns a net.Listener that accepts only the
	// connections that matched by at least of the matcher writers.
	//
	// Prefer Matchers over MatchWriters, since the latter can write on the
	// connection before the actual handler.
	//
	// The order used to call Match determines the priority of matchers.
	MatchWithWriters(...MatchWriter) net.Listener
	// Serve starts multiplexing the listener. Serve blocks and perhaps
	// should be invoked concurrently within a go routine.
	Serve() error
	// HandleError registers an error handler that handles listener errors.
	HandleError(ErrorHandler)
	// sets a timeout for the read of matchers
	SetReadTimeout(time.Duration)
	// Close cmux server and stop listen
	Close()
}

type matchersListener struct {
	ss []MatchWriter
	l  muxListener
}

type cMux struct {
	root        net.Listener
	bufLen      int
	errh        ErrorHandler
	donec       chan struct{}
	sls         []matchersListener
	readTimeout time.Duration
	allc        []net.Conn
}

func matchersToMatchWriters(matchers []Matcher) []MatchWriter {
	mws := make([]MatchWriter, 0, len(matchers))
	for _, m := range matchers {
		cm := m
		mws = append(mws, func(w io.Writer, r io.Reader) bool {
			return cm(r)
		})
	}
	return mws
}

func (m *cMux) Match(matchers ...Matcher) net.Listener {
	mws := matchersToMatchWriters(matchers)
	return m.MatchWithWriters(mws...)
}

func (m *cMux) MatchWithWriters(matchers ...MatchWriter) net.Listener {
	ml := muxListener{
		Listener: m.root,
		connc:    make(chan net.Conn, m.bufLen),
	}
	m.sls = append(m.sls, matchersListener{ss: matchers, l: ml})
	return ml
}

func (m *cMux) SetReadTimeout(t time.Duration) {
	m.readTimeout = t
}

func (m *cMux) Serve() error {
	var wg sync.WaitGroup

	defer func() {
		m.closeDoneChan()
		wg.Wait()

		for _, sl := range m.sls {
			close(sl.l.connc)
			// Drain the connections enqueued for the listener.
			for c := range sl.l.connc {
				_ = c.Close()
			}
		}
	}()

	for {
		select {
		case <-m.donec:
			// cmux was closed by Close()
			return nil
		default:
			// pass
		}
		c, err := m.root.Accept()
		if err != nil {
			if !m.handleErr(err) {
				return err
			}
			continue
		}

		select {
		case <-m.donec:
			// cmux was closed by Close()
			return nil
		default:
			// pass
		}

		wg.Add(1)
		// TODO: 在本系统中，连接有限，所以可以这么干
		// 本质上这里有泄露，c被正常close时没有从slice中清理
		m.allc = append(m.allc, c)
		go m.serve(c, m.donec, &wg)
	}
}

func (m *cMux) Close() {
	m.closeDoneChan()
	// close all connected connections
	var wg sync.WaitGroup
	for _, c := range m.allc {
		wg.Add(1)
		go func(c net.Conn) {
			c.Close()
			wg.Done()
		}(c)
	}
	wg.Wait()
	// close Listen
	m.root.Close()
}

func (m *cMux) closeDoneChan() {
	select {
	case <-m.donec:
		// already closed
	default:
		close(m.donec)
	}
}

func (m *cMux) serve(c net.Conn, donec <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	muc := newMuxConn(c)
	if m.readTimeout > noTimeout {
		_ = c.SetReadDeadline(time.Now().Add(m.readTimeout))
	}
	for _, sl := range m.sls {
		for _, s := range sl.ss {
			matched := s(muc.Conn, muc.startSniffing())
			if matched {
				muc.doneSniffing()
				if m.readTimeout > noTimeout {
					_ = c.SetReadDeadline(time.Time{})
				}
				select {
				case sl.l.connc <- muc:
				case <-donec:
					_ = c.Close()
				}
				return
			}
		}
	}

	_ = c.Close()
	err := ErrNotMatched{c: c}
	if !m.handleErr(err) {
		_ = m.root.Close()
	}
}

func (m *cMux) HandleError(h ErrorHandler) {
	m.errh = h
}

func (m *cMux) handleErr(err error) bool {
	if !m.errh(err) {
		return false
	}

	if ne, ok := err.(net.Error); ok {
		return ne.Temporary()
	}

	return false
}

type muxListener struct {
	net.Listener
	connc chan net.Conn
}

func (l muxListener) Accept() (net.Conn, error) {
	c, ok := <-l.connc
	if !ok {
		return nil, ErrListenerClosed
	}
	return c, nil
}

// MuxConn wraps a net.Conn and provides transparent sniffing of connection data.
type MuxConn struct {
	net.Conn
	buf bufferedReader
}

func newMuxConn(c net.Conn) *MuxConn {
	return &MuxConn{
		Conn: c,
		buf:  bufferedReader{source: c},
	}
}

// From the io.Reader documentation:
//
// When Read encounters an error or end-of-file condition after
// successfully reading n > 0 bytes, it returns the number of
// bytes read.  It may return the (non-nil) error from the same call
// or return the error (and n == 0) from a subsequent call.
// An instance of this general case is that a Reader returning
// a non-zero number of bytes at the end of the input stream may
// return either err == EOF or err == nil.  The next Read should
// return 0, EOF.
func (m *MuxConn) Read(p []byte) (int, error) {
	return m.buf.Read(p)
}

func (m *MuxConn) startSniffing() io.Reader {
	m.buf.reset(true)
	return &m.buf
}

func (m *MuxConn) doneSniffing() {
	m.buf.reset(false)
}
