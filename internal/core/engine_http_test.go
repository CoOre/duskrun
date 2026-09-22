package core

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestHTTPServerBoundsClientControlledPhases pins the timeouts rather than
// asserting behaviour, because the failure they prevent takes minutes to
// reproduce and the regression is silent: dropping a field compiles, passes
// every functional test, and only shows up as goroutines piling up in
// production.
//
// WriteTimeout must stay zero. It is the one field that would break correct
// behaviour: SSE run streams and artifact downloads are legitimately long, and
// a response deadline severs them mid-flight.
func TestHTTPServerBoundsClientControlledPhases(t *testing.T) {
	srv := newHTTPServer(":8080", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is unset — a slowloris client holds a goroutine indefinitely")
	}
	if srv.ReadTimeout <= 0 {
		t.Error("ReadTimeout is unset — a body can arrive one byte at a time forever")
	}
	if srv.IdleTimeout <= 0 {
		t.Error("IdleTimeout is unset — kept-alive connections are never reclaimed")
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Error("MaxHeaderBytes is unset")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: it would cut off SSE streams and artifact downloads",
			srv.WriteTimeout)
	}
}

// TestSlowHeaderClientIsDisconnected is the behavioural half: a client that
// opens a connection and never finishes its headers must be dropped by the
// server rather than held forever.
func TestSlowHeaderClientIsDisconnected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer("", http.NewServeMux())
	// The real value is 10s; a test must not take that long, and the field
	// under test is the one being overridden, so this still exercises the path.
	srv.ReadHeaderTimeout = 200 * time.Millisecond
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A request line and one header, then silence — never the blank line that
	// would end the header block.
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}

	// The server should give up well inside this deadline; without a header
	// timeout the read below would block until the test binary is killed.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadByte(); err == nil {
		// A 408 body is also an acceptable outcome; what matters is that the
		// server acted. Only a read deadline of our own means it did not.
		return
	} else if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		t.Fatal("server never closed a connection with unfinished headers")
	}
}
