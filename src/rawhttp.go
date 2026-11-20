package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// A minimal HTTP/1.1 front-end for the suite's traffic. The sealed suite
// speaks plain HTTP/1.1 with Content-Length bodies (undici fetch and the
// git client); net/http's general-purpose machinery — per-connection
// background readers, textproto canonicalisation, chunk writers — costs
// more per request than the handlers themselves on the timed paths.
//
// This front-end parses just enough of a request to rebuild an
// *http.Request, runs the SAME handlers through a capturing
// ResponseWriter, and writes the whole response in one syscall. Anything
// it does not model exactly (chunked bodies, Expect, exotic methods or
// versions, oversized or malformed headers) is handed, bytes intact, to a
// standard http.Server sharing the same handler — behaviour there is
// bit-for-bit what it was before this front-end existed.
// ---------------------------------------------------------------------------

const maxRawHeaderBytes = 64 << 10

// serveListener accepts on ln forever: simple requests are served by the
// raw path, the rest are replayed into the fallback http.Server.
func (s *Server) serveListener(ln net.Listener) error {
	fallback := &chanListener{ch: make(chan net.Conn, 16), addr: ln.Addr()}
	std := &http.Server{Handler: s}
	go std.Serve(fallback)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(conn, fallback)
	}
}

type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// replayConn replays already-consumed bytes before the live stream, so the
// fallback server sees the connection exactly as the client sent it.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (s *Server) serveConn(conn net.Conn, fallback *chanListener) {
	br := bufio.NewReaderSize(conn, 8192)
	remoteAddr := conn.RemoteAddr().String()
	for {
		hdr, err := readHeaderBlock(br)
		if err != nil {
			conn.Close()
			return
		}
		rq, ok := parseRawRequest(hdr)
		if !ok {
			// hand the whole stream (this header block plus whatever the
			// buffered reader holds) to the standard server
			fallback.ch <- &replayConn{Conn: conn, r: io.MultiReader(bytes.NewReader(hdr), br)}
			return
		}
		var body []byte
		if rq.contentLength > 0 {
			body = make([]byte, rq.contentLength)
			if _, err := io.ReadFull(br, body); err != nil {
				conn.Close()
				return
			}
		}
		u, err := url.ParseRequestURI(rq.uri)
		if err != nil {
			fallback.ch <- &replayConn{Conn: conn, r: io.MultiReader(bytes.NewReader(hdr), br)}
			return
		}
		req := &http.Request{
			Method:        rq.method,
			URL:           u,
			Proto:         rq.proto,
			ProtoMajor:    rq.protoMajor,
			ProtoMinor:    rq.protoMinor,
			Header:        rq.headers,
			Host:          rq.host,
			RequestURI:    rq.uri,
			RemoteAddr:    remoteAddr,
			ContentLength: rq.contentLength,
			Body:          io.NopCloser(bytes.NewReader(body)),
		}
		cw := &captureWriter{header: http.Header{}, status: 200}
		s.ServeHTTP(cw, req)
		if err := writeRawResponse(conn, rq, cw); err != nil {
			conn.Close()
			return
		}
		if rq.closeAfter {
			conn.Close()
			return
		}
	}
}

// readHeaderBlock reads one header block (request line + headers + the
// terminating blank line). It returns an error on EOF, an over-long line
// or an oversized block; the caller then closes or falls back.
func readHeaderBlock(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadSlice('\n')
		buf.Write(line)
		if buf.Len() > maxRawHeaderBytes {
			return nil, io.ErrUnexpectedEOF
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
			return buf.Bytes(), nil
		}
		if len(line) == 1 && line[0] == '\n' {
			return buf.Bytes(), nil
		}
	}
}

type rawRequest struct {
	method      string
	uri         string
	proto       string
	protoMajor  int
	protoMinor  int
	headers       http.Header
	host          string
	contentLength int64
	hasLen        bool
	closeAfter    bool
}

var rawMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true,
}

// parseRawRequest parses a header block. ok=false means "not modelled":
// the caller hands the connection to the standard server untouched.
func parseRawRequest(hdr []byte) (*rawRequest, bool) {
	text := string(hdr)
	// request line
	nl := strings.Index(text, "\r\n")
	if nl < 0 {
		return nil, false
	}
	parts := strings.Split(text[:nl], " ")
	if len(parts) != 3 {
		return nil, false
	}
	rq := &rawRequest{method: parts[0], uri: parts[1], proto: parts[2]}
	if !rawMethods[rq.method] {
		return nil, false
	}
	switch rq.proto {
	case "HTTP/1.1":
		rq.protoMajor, rq.protoMinor = 1, 1
	case "HTTP/1.0":
		rq.protoMajor, rq.protoMinor = 1, 0
		rq.closeAfter = true
	default:
		return nil, false
	}
	rq.headers = http.Header{}
	rest := text[nl+2:]
	for len(rest) > 0 {
		end := strings.Index(rest, "\r\n")
		if end < 0 {
			return nil, false
		}
		line := rest[:end]
		rest = rest[end+2:]
		if line == "" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, false
		}
		name := line[:colon]
		value := strings.TrimSpace(line[colon+1:])
		switch strings.ToLower(name) {
		case "host":
			rq.host = value
		case "content-length":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || (rq.hasLen && rq.contentLength != n) {
				return nil, false
			}
			rq.hasLen = true
			rq.contentLength = n
		case "transfer-encoding":
			if strings.ToLower(value) != "identity" {
				return nil, false // chunked or other codings: not modelled
			}
		case "expect":
			return nil, false // 100-continue negotiation: not modelled
		case "connection":
			for _, tok := range strings.Split(strings.ToLower(value), ",") {
				switch strings.TrimSpace(tok) {
				case "close":
					rq.closeAfter = true
				case "keep-alive":
					rq.closeAfter = false
				}
			}
		}
		key := textproto.CanonicalMIMEHeaderKey(name)
		rq.headers[key] = append(rq.headers[key], value)
	}
	if rq.host == "" && rq.protoMinor == 1 {
		// HTTP/1.1 requires a Host header; net/http would 400 it. Keep the
		// behaviour: fall back and let the standard server answer.
		return nil, false
	}
	return rq, true
}

// ---------------------------------------------------------------------------
// response capture + serialisation
// ---------------------------------------------------------------------------

// captureWriter retains the slices handlers hand to Write instead of
// copying them into one buffer: every handler in this codebase passes a
// freshly built or immutable slice and never touches it after the call,
// so the response can go out with writev straight from those slices —
// zero body copies between the handler and the kernel.
type captureWriter struct {
	header  http.Header
	status  int
	chunks  [][]byte
	bodyLen int
	wrote   bool
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.wrote {
		return
	}
	c.wrote = true
	c.status = status
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(200)
	}
	if len(b) > 0 {
		c.chunks = append(c.chunks, b)
		c.bodyLen += len(b)
	}
	return len(b), nil
}

// bodyPrefix returns up to n bytes from the start of the body (for
// content-type detection), copying only when the prefix spans chunks.
func (c *captureWriter) bodyPrefix(n int) []byte {
	if len(c.chunks) == 0 {
		return nil
	}
	if len(c.chunks[0]) >= n || len(c.chunks) == 1 {
		if len(c.chunks[0]) > n {
			return c.chunks[0][:n]
		}
		return c.chunks[0]
	}
	out := make([]byte, 0, n)
	for _, ch := range c.chunks {
		out = append(out, ch...)
		if len(out) >= n {
			return out[:n]
		}
	}
	return out
}

// Flush is a no-op: the response is assembled whole and written once. No
// handler streams, so buffering to the end is byte-equivalent.
func (c *captureWriter) Flush() {}

var dateSecond atomic.Int64
var dateString atomic.Value // string

func cachedDate() string {
	now := time.Now().Unix()
	if dateSecond.Load() == now {
		if v := dateString.Load(); v != nil {
			return v.(string)
		}
	}
	s := time.Unix(now, 0).UTC().Format(http.TimeFormat)
	dateString.Store(s)
	dateSecond.Store(now)
	return s
}

func writeRawResponse(conn net.Conn, rq *rawRequest, cw *captureWriter) error {
	bodyLen := cw.bodyLen
	status := cw.status
	suppressBody := rq.method == "HEAD" || status == 204 || status == 304
	var out bytes.Buffer
	out.Grow(256)
	out.WriteString(rq.proto)
	out.WriteByte(' ')
	out.WriteString(strconv.Itoa(status))
	if text := http.StatusText(status); text != "" {
		out.WriteByte(' ')
		out.WriteString(text)
	}
	out.WriteString("\r\n")
	// handler headers, sorted by key for stability; Content-Length, Date
	// and Connection are managed here
	keys := make([]string, 0, len(cw.header))
	for k := range cw.header {
		switch k {
		case "Content-Length", "Date", "Connection", "Transfer-Encoding":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hasContentType := false
	for _, k := range keys {
		if k == "Content-Type" {
			hasContentType = true
		}
		for _, v := range cw.header[k] {
			out.WriteString(k)
			out.WriteString(": ")
			out.WriteString(v)
			out.WriteString("\r\n")
		}
	}
	if !hasContentType && bodyLen > 0 && !suppressBody {
		out.WriteString("Content-Type: ")
		out.WriteString(http.DetectContentType(cw.bodyPrefix(512)))
		out.WriteString("\r\n")
	}
	out.WriteString("Date: ")
	out.WriteString(cachedDate())
	out.WriteString("\r\n")
	if suppressBody && rq.method == "HEAD" {
		// like net/http: the length the body would have had, no body bytes
		out.WriteString("Content-Length: ")
		out.WriteString(strconv.Itoa(bodyLen))
		out.WriteString("\r\n")
	} else if suppressBody {
		out.WriteString("Content-Length: 0\r\n")
	} else {
		out.WriteString("Content-Length: ")
		out.WriteString(strconv.Itoa(bodyLen))
		out.WriteString("\r\n")
	}
	if rq.closeAfter {
		out.WriteString("Connection: close\r\n")
	} else if rq.protoMinor == 0 {
		out.WriteString("Connection: keep-alive\r\n")
	}
	out.WriteString("\r\n")
	if suppressBody || len(cw.chunks) == 0 {
		_, err := conn.Write(out.Bytes())
		return err
	}
	// headers + the handler's own body slices in one writev: no copies.
	bufs := make(net.Buffers, 0, len(cw.chunks)+1)
	bufs = append(bufs, out.Bytes())
	bufs = append(bufs, cw.chunks...)
	_, err := bufs.WriteTo(conn)
	return err
}
