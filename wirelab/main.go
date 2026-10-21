package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------
// The instruments. Read these after Experiment 1, not before.
// ---------------------------------------------------------------------

// tapConn sits just above the operating system. Every Write on it is one
// call that net/http makes to put bytes on the socket.
type tapConn struct{ net.Conn }

func (c tapConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	log.Printf("SOCKET   conn.Write: %5d bytes  starts %q", n, preview(p, 34))
	return n, err
}

func (c tapConn) Close() error {
	log.Printf("SOCKET   connection closed")
	return c.Conn.Close()
}

// CloseWrite lets net/http finish a connection politely.
func (c tapConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

type tapListener struct{ net.Listener }

func (l tapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	log.Printf("SOCKET   connection accepted")
	return tapConn{c}, nil
}

func preview(p []byte, n int) string {
	if len(p) > n {
		p = p[:n]
	}
	return string(p)
}

// tapWriter sits just under your handler. It is the w your handler receives.
// It logs what the handler does, then passes everything on to the real w.
type tapWriter struct {
	http.ResponseWriter
	calls, bytes int
	statusFixed  bool
}

const loggedWrites = 4 // log the first few Write calls one by one

func (t *tapWriter) WriteHeader(code int) {
	if t.statusFixed {
		log.Printf("HANDLER  w.WriteHeader(%d)  TOO LATE: the status was already fixed", code)
	} else {
		log.Printf("HANDLER  w.WriteHeader(%d)  status fixed here", code)
		t.statusFixed = true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *tapWriter) Write(p []byte) (int, error) {
	if !t.statusFixed {
		log.Printf("HANDLER  first Write: net/http calls WriteHeader(200) for you; status fixed here")
		t.statusFixed = true
	}
	t.calls++
	t.bytes += len(p)
	switch {
	case t.calls <= loggedWrites:
		log.Printf("HANDLER  w.Write #%d: %d bytes", t.calls, len(p))
	case t.calls == loggedWrites+1:
		log.Printf("HANDLER  (more Write calls follow, not logged one by one)")
	}
	return t.ResponseWriter.Write(p)
}

// FlushError is what http.NewResponseController calls to flush.
func (t *tapWriter) FlushError() error {
	log.Printf("HANDLER  Flush() requested")
	return http.NewResponseController(t.ResponseWriter).Flush()
}

func (t *tapWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func tapped(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("HANDLER  ---- %s starts ----", r.URL.RequestURI())
		tw := &tapWriter{ResponseWriter: w}
		h(tw, r)
		log.Printf("HANDLER  ---- handler returns: %d Write calls, %d bytes total ----", tw.calls, tw.bytes)
	}
}

func note(format string, args ...any) {
	log.Printf("HANDLER  "+format, args...)
}

// intParam reads a positive integer query parameter, clamped to [1, max].
func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 1 {
		return def
	}
	return min(v, max)
}

// ---------------------------------------------------------------------
// The experiments.
// ---------------------------------------------------------------------

// GET /hello: one small Write.
func hello(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "hello from the lab server")
}

// GET /drip?size=1000&count=8&pause=300: count Writes of size bytes each,
// pausing pause milliseconds between them, so you can watch in slow motion.
func drip(w http.ResponseWriter, r *http.Request) {
	size := intParam(r, "size", 1000, 1<<20)
	count := intParam(r, "count", 8, 100)
	pause := time.Duration(intParam(r, "pause", 300, 5000)) * time.Millisecond
	for i := 0; i < count; i++ {
		w.Write(bytes.Repeat([]byte{byte('a' + i%26)}, size))
		time.Sleep(pause)
	}
}

// GET /pieces?n=1000: n separate Write calls of 10 bytes each.
func pieces(w http.ResponseWriter, r *http.Request) {
	n := intParam(r, "n", 1000, 100000)
	for i := 0; i < n; i++ {
		fmt.Fprint(w, "0123456789")
	}
}

// GET /big?kb=64: ONE Write call of kb kilobytes.
func big(w http.ResponseWriter, r *http.Request) {
	kb := intParam(r, "kb", 64, 4096)
	w.Write(bytes.Repeat([]byte("x"), kb*1024))
}

// GET /hold?flush=0&pause=3000: write 100 bytes, wait, then return.
// With flush=1 the handler calls Flush() right after writing.
func hold(w http.ResponseWriter, r *http.Request) {
	pause := time.Duration(intParam(r, "pause", 3000, 10000)) * time.Millisecond
	w.Write(bytes.Repeat([]byte("h"), 100))
	if r.URL.Query().Get("flush") == "1" {
		http.NewResponseController(w).Flush()
	}
	note("sleeping %v", pause)
	time.Sleep(pause)
}

// GET /late-header: sets one header before the first Write and one after.
func lateHeader(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Before", "set before the first Write")
	fmt.Fprintln(w, "body")
	note("now setting X-After, after the Write")
	w.Header().Set("X-After", "set after the first Write")
}

// GET /late-error: writes part of a page, THEN reports an error.
func lateError(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "<html><body><h1>Report</h1>")
	err := errors.New("template failed halfway")
	if err != nil {
		note("error found: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

// GET /safe-error: builds the page in our own buffer and reports the error
// BEFORE touching w.
func safeError(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	buf.WriteString("<html><body><h1>Report</h1>")
	err := errors.New("template failed halfway")
	if err != nil {
		note("error found: %v (nothing written to w yet)", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", tapped(hello))
	mux.HandleFunc("GET /drip", tapped(drip))
	mux.HandleFunc("GET /pieces", tapped(pieces))
	mux.HandleFunc("GET /big", tapped(big))
	mux.HandleFunc("GET /hold", tapped(hold))
	mux.HandleFunc("GET /late-header", tapped(lateHeader))
	mux.HandleFunc("GET /late-error", tapped(lateError))
	mux.HandleFunc("GET /safe-error", tapped(safeError))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("lab server listening on %s", ln.Addr())
	log.Fatal(srv.Serve(tapListener{ln}))
}
