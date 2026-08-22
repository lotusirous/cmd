package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// urlMark stands in for the test server's address in the files below, so
// each one reads like a file you would write by hand.
const urlMark = "{{url}}"

// serve starts a test server whose responses carry no Date header, so the
// files below stay byte-for-byte stable from run to run.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testClient returns srv's client with the same redirect policy rr uses.
func testClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = noRedirect
	return c
}

// write creates an rr file holding content, with {{url}} replaced by the
// address srv is listening on.
func write(t *testing.T, srv *httptest.Server, content string) string {
	t.Helper()
	if srv != nil {
		content = strings.ReplaceAll(content, urlMark, srv.URL)
	}
	path := filepath.Join(t.TempDir(), "req.rr")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// canon puts the address back as {{url}} and folds the CRLF that wire format
// requires down to LF, so what a test reads compares against the plain text it
// was written with. TestStoredResponseIsWireFormat covers the folding.
func canon(srv *httptest.Server, s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if srv != nil {
		s = strings.ReplaceAll(s, srv.URL, urlMark)
	}
	return s
}

func read(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return canon(srv, string(data))
}

// sections parses text the way rr does, so a test can look at one half of one
// exchange without cutting the text itself.
func sections(t *testing.T, text string) []section {
	t.Helper()
	_, secs, err := splitFile([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return secs
}

// stdoutFor returns what run should print for the exchanges in want, which is
// each response under the marker naming it.
func stdoutFor(t *testing.T, want string) string {
	t.Helper()
	var b strings.Builder
	for _, s := range sections(t, want) {
		if len(s.resp) > 0 {
			b.WriteString(marker(s.name))
			b.Write(s.resp)
		}
	}
	return b.String()
}

// TestRun checks the whole round trip: file in, file out. Each want is the
// complete file rr should leave behind, and stdout must be its responses.
func TestRun(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		file    string
		want    string
	}{
		{
			name: "request headers are sent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Test") != "yes" {
					http.Error(w, "bad header", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com
X-Test: yes

`,
			want: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com
X-Test: yes

HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello
`,
		},
		{
			name: "json body is indented",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true,"n":1}`)
			},
			file: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com
Content-Length: 4

body`,
			want: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com

body
HTTP/1.1 200 OK
Content-Length: 26
Content-Type: application/json

{
  "ok": true,
  "n": 1
}
`,
		},
		{
			name: "an old response is replaced",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "v2")
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

v1
`,
			want: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

v2
`,
		},
		{
			// A chunked response has no length to preserve, so it is stored
			// with the length of the body actually written, unframed.
			name: "a chunked response is stored plainly",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "chun")
				w.(http.Flusher).Flush()
				io.WriteString(w, "ked")
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

`,
			want: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

HTTP/1.1 200 OK
Content-Length: 7
Content-Type: text/plain

chunked
`,
		},
		{
			// A hand-written exchange often stops after the last header. The
			// header block is unterminated, but unambiguous: rr completes it
			// and stores the file in canonical form.
			name: "an exchange that ends after the last header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com`,
			want: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello
`,
		},
		{
			// http.ReadRequest would drop this body for want of framing.
			name:    "a body needs no Content-Length",
			handler: echoBody,
			file: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com
Content-Type: application/json

{"a":1}`,
			want: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com
Content-Type: application/json

{
  "a": 1
}
HTTP/1.1 200 OK
Content-Length: 16
Content-Type: text/plain

got {
  "a": 1
}
`,
		},
		{
			// ... and would send only the first two bytes of it here.
			name:    "a stale Content-Length is dropped",
			handler: echoBody,
			file: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com
Content-Length: 2

{"a":1}`,
			want: `-- x --
POST {{url}}/ HTTP/1.1
Host: example.com

{"a":1}
HTTP/1.1 200 OK
Content-Length: 11
Content-Type: text/plain

got {"a":1}
`,
		},
		{
			// The body parses as JSON, but text/plain says it is not one.
			name: "a response is indented only when it says it is json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, `{"a":1}`)
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

`,
			want: `-- x --
GET {{url}}/ HTTP/1.1
Host: example.com

HTTP/1.1 200 OK
Content-Length: 7
Content-Type: text/plain

{"a":1}
`,
		},
		{
			// An absolute request URI carries the host, so Host is optional.
			name: "an absolute URI needs no Host header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			},
			file: `-- x --
GET {{url}}/ HTTP/1.1

`,
			want: `-- x --
GET {{url}}/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(t, tt.handler)
			path := write(t, srv, tt.file)

			var stdout bytes.Buffer
			if err := run(t.Context(), path, option{client: testClient(srv), out: &stdout}); err != nil {
				t.Fatal(err)
			}

			if got := read(t, srv, path); got != tt.want {
				t.Errorf("file after run:\n%s\n\nwant:\n%s", got, tt.want)
			}
			if got, want := canon(srv, stdout.String()), stdoutFor(t, tt.want); got != want {
				t.Errorf("stdout:\n%s\n\nwant:\n%s", got, want)
			}
		})
	}
}

// A file is a collection: rr sends every exchange in it, in the order the file
// has them, and stores each answer under the request that made it.
func TestRunSendsEveryExchange(t *testing.T) {
	var order []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.TrimPrefix(r.URL.Path, "/"))
	})

	file := `-- one --
GET {{url}}/a HTTP/1.1

-- two --
GET {{url}}/b HTTP/1.1

`
	want := `-- one --
GET {{url}}/a HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 1
Content-Type: text/plain

a
-- two --
GET {{url}}/b HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 1
Content-Type: text/plain

b
`
	path := write(t, srv, file)
	var stdout bytes.Buffer
	if err := run(t.Context(), path, option{client: testClient(srv), out: &stdout}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"/a", "/b"}) {
		t.Errorf("sent %q, want the file's order [/a /b]", order)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
	if got, w := canon(srv, stdout.String()), stdoutFor(t, want); got != w {
		t.Errorf("stdout:\n%s\n\nwant:\n%s", got, w)
	}
}

// -match names the exchanges to send. The rest are not sent and are stored
// back exactly as the file had them, unformatted request and all.
func TestRunMatch(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})

	file := `-- repos/list --
GET {{url}}/a HTTP/1.1

-- users/list --
get {{url}}/b HTTP/1.1

`
	want := `-- repos/list --
GET {{url}}/a HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

ok
-- users/list --
get {{url}}/b HTTP/1.1

`
	path := write(t, srv, file)
	if err := run(t.Context(), path, option{
		match:  regexp.MustCompile("^repos/"),
		client: testClient(srv),
		out:    io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
}

// A pattern that names nothing is an error: running nothing and running
// everything unchanged leave the same clean git diff behind.
func TestRunNoMatch(t *testing.T) {
	file := "-- x --\nGET https://example.com/ HTTP/1.1\n\n"
	path := write(t, nil, file)

	err := run(t.Context(), path, option{
		match:  regexp.MustCompile("nope"),
		client: http.DefaultClient,
		out:    io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "no exchange matches") {
		t.Fatalf("err = %v, want no exchange matches", err)
	}
	if got := read(t, nil, path); got != file {
		t.Errorf("file changed:\n%s\n\nwant:\n%s", got, file)
	}
}

// A Date that moves every run and a request id that is new every time say
// nothing about an API and everything about the clock: -omit names them by
// header, and rr stores the answer without them.
func TestRunOmit(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Date", "Mon, 01 Jan 2035 00:00:00 GMT")
		w.Header().Set("X-Amz-Request-Id", "7f3c")
		w.Header().Set("X-Request-Id", "abcd")
		io.WriteString(w, "ok")
	})

	file := `-- x --
GET {{url}}/ HTTP/1.1

`
	want := `-- x --
GET {{url}}/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain
X-Request-Id: abcd

ok
`
	path := write(t, srv, file)
	var stdout bytes.Buffer
	omit := regexp.MustCompile("^(Date|X-Amz-)")
	if err := run(t.Context(), path, option{
		omit:   omit,
		client: testClient(srv),
		out:    &stdout,
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
	if got, w := canon(srv, stdout.String()), stdoutFor(t, want); got != w {
		t.Errorf("stdout:\n%s\n\nwant:\n%s", got, w)
	}
}

// Content-Length is rr's own framing of the body it stores rather than a
// header the server sent, so it outlives a pattern that matches every name.
// The body is shaped before the names are dropped, too: what the server said
// its body was decides how the body is stored, whether or not the file keeps
// the saying.
func TestRunOmitKeepsFraming(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":7}`)
	})

	want := `-- x --
GET {{url}}/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 13

{
  "id": 7
}
`
	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\n\n")
	if err := run(t.Context(), path, option{
		omit:   regexp.MustCompile(".*"),
		client: testClient(srv),
		out:    io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
}

// The request is the writer's own text, and rr deletes no line of it: -omit
// is about the answer, which is rr's to write.
func TestRunOmitLeavesRequest(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Date", "Mon, 01 Jan 2035 00:00:00 GMT")
		io.WriteString(w, "ok")
	})

	file := `-- x --
GET {{url}}/ HTTP/1.1
Date: Mon, 01 Jan 2035 00:00:00 GMT
X-Request-Id: 7

`
	want := file + `HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

ok
`
	path := write(t, srv, file)
	omit := regexp.MustCompile("^(Date|X-Request-Id)$")
	if err := run(t.Context(), path, option{
		omit:   omit,
		client: testClient(srv),
		out:    io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
}

// Rr stops at the first failure, an exchange being free to rely on the one
// above it. What answered before it is stored; the rest of the file is left
// as it was, so the diff says how far the run got.
func TestRunStopsAtFirstFailure(t *testing.T) {
	var sent int
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})

	file := `-- a --
GET {{url}}/ HTTP/1.1

-- b --
GET http://127.0.0.1:1/nope HTTP/1.1

-- c --
GET {{url}}/ HTTP/1.1

`
	want := `-- a --
GET {{url}}/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

ok
-- b --
GET http://127.0.0.1:1/nope HTTP/1.1

-- c --
GET {{url}}/ HTTP/1.1

`
	path := write(t, srv, file)
	err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard})

	var perr *fs.PathError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want an *fs.PathError", err)
	}
	if perr.Path != path {
		t.Errorf("err names %s, want %s", perr.Path, path)
	}
	if !strings.Contains(perr.Err.Error(), "b:") {
		t.Errorf("err = %v, want it to name the exchange b", perr.Err)
	}
	if sent != 1 {
		t.Errorf("the server saw %d requests, want 1", sent)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
}

// Text before the first marker is a comment: run does not read it, and it
// survives every rewrite.
func TestRunKeepsComment(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})

	file := `what this file is for

-- x --
GET {{url}}/ HTTP/1.1

`
	path := write(t, srv, file)
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, srv, path); !strings.HasPrefix(got, "what this file is for\n\n-- x --\n") {
		t.Errorf("comment lost:\n%s", got)
	}
}

// TestRunErrors checks that a failed run says why and leaves the file alone.
func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{
			name: "request URI is not absolute",
			file: `-- x --
GET /path HTTP/1.1
Host: example.com

`,
			want: "absolute",
		},
		{
			name: "every unset variable is named",
			file: `-- x --
GET https://example.com/$ALSO HTTP/1.1
Host: example.com
Authorization: Bearer ${MISSING}

`,
			want: "unset: ALSO, MISSING",
		},
		{
			name: "the server cannot be reached",
			file: `-- x --
GET http://127.0.0.1:1/nope HTTP/1.1
Host: 127.0.0.1:1

HTTP/1.1 500 Old
Content-Length: 3

old
`,
			want: "127.0.0.1:1",
		},
		{
			// RFC 9112, 3.2: a request with two Host lines is malformed, and
			// http.ReadRequest refuses it rather than pick one.
			name: "two Host headers",
			file: `-- x --
GET https://example.com/ HTTP/1.1
Host: one
Host: two

`,
			want: "too many Host headers",
		},
		{
			name: "an empty file",
			file: "",
			want: "no exchange",
		},
		{
			name: "a file with no marker",
			file: "GET https://example.com/ HTTP/1.1\n\n",
			want: "no exchange",
		},
		{
			name: "two exchanges of one name",
			file: "-- x --\nGET https://example.com/a HTTP/1.1\n\n-- x --\nGET https://example.com/b HTTP/1.1\n\n",
			want: "two exchanges named x",
		},
		{
			name: "an exchange with no name",
			file: "--  --\nGET https://example.com/ HTTP/1.1\n\n",
			want: "no name",
		},
		{
			name: "an exchange that is all response",
			file: "-- x --\nHTTP/1.1 200 OK\nContent-Length: 0\n\n",
			want: "exchange x has no request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, nil, tt.file)

			err := run(t.Context(), path, option{
				client: &http.Client{CheckRedirect: noRedirect},
				out:    io.Discard,
			})

			// Every error is an *fs.PathError naming the file. Matching on
			// the inner error also keeps the temporary path, which embeds
			// this subtest's name, from matching in place of the message.
			var perr *fs.PathError
			if !errors.As(err, &perr) {
				t.Fatalf("err = %v, want an *fs.PathError", err)
			}
			if perr.Path != path {
				t.Errorf("err names %s, want %s", perr.Path, path)
			}
			if !strings.Contains(perr.Err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", perr.Err, tt.want)
			}
			if got := read(t, nil, path); got != tt.file {
				t.Errorf("file changed on error:\n%s\n\nwant:\n%s", got, tt.file)
			}
		})
	}
}

// echoBody answers with the body it was sent, so a test can see what rr
// actually put on the wire.
func echoBody(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "got %s", b)
}

// Rr fmt formats the request half without sending anything: it takes no
// client, and a stored response is left byte for byte.
func TestFormatFile(t *testing.T) {
	before := `-- x --
get https://example.com/ HTTP/1.1
content-type:application/json
content-length: 2

{"a":1}
HTTP/1.1 200 OK
Content-Length: 2

hi
`
	after := `-- x --
GET https://example.com/ HTTP/1.1
Content-Type: application/json

{
  "a": 1
}
HTTP/1.1 200 OK
Content-Length: 2

hi
`

	path := write(t, nil, before)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, nil, path); got != after {
		t.Errorf("formatted:\n%s\nwant:\n%s", got, after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

// A marker is written the one way it is written, a name being the same name
// however it was spaced, and the comment above the first one is kept.
func TestFormatFileMarkers(t *testing.T) {
	path := write(t, nil, "notes\n\n--   a/b   --\nget https://example.com/ HTTP/1.1\n")
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	want := "notes\n\n-- a/b --\nGET https://example.com/ HTTP/1.1\n\n"
	if got := read(t, nil, path); got != want {
		t.Errorf("formatted:\n%q\nwant:\n%q", got, want)
	}
}

// Formatting a formatted file changes nothing, so fmt in a loop is a fixed
// point rather than a diff.
func TestFormatFileIsIdempotent(t *testing.T) {
	path := write(t, nil, "notes\n\n-- a --\nget https://example.com/ HTTP/1.1\nx-k:1\n\n{\"a\":1}\n-- b --\nGET https://example.com/b HTTP/1.1\n\n")
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	once := read(t, nil, path)
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	if twice := read(t, nil, path); twice != once {
		t.Errorf("formatting twice differs:\n%s\nwant:\n%s", twice, once)
	}
}

// An exchange with no response yet gains no response.
func TestFormatFileNoResponse(t *testing.T) {
	path := write(t, nil, "-- x --\nget https://example.com/ HTTP/1.1\nhost: h")
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	want := "-- x --\nGET https://example.com/ HTTP/1.1\nHost: h\n\n"
	if got := read(t, nil, path); got != want {
		t.Errorf("formatted:\n%q\nwant:\n%q", got, want)
	}
}

// Running a file formats it, and leaves the mode alone.
func TestRunFormats(t *testing.T) {
	srv := serve(t, echoBody)
	before := `-- x --
post {{url}}/ HTTP/1.1
content-type:application/json
content-length: 2
X-EMPTY:	

{"a":1}`
	after := `POST {{url}}/ HTTP/1.1
Content-Type: application/json
X-EMPTY:

{
  "a": 1
}
`

	path := write(t, srv, before)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	if req := string(sections(t, read(t, srv, path))[0].req); req != after {
		t.Errorf("request half:\n%s\nwant:\n%s", req, after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

// The newline that ends the file is the file's, not the body's, so running
// twice sends the same request both times.
func TestRunTwiceSendsSameBody(t *testing.T) {
	seen := make(chan string, 2)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- string(b)
	})

	// No trailing newline: rr adds one when it stores the file.
	path := write(t, srv, "-- x --\nPOST "+urlMark+"/ HTTP/1.1\nHost: h\n\n{\"a\":1}")
	for i := range 2 {
		if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	want := `{"a":1}`
	first, second := <-seen, <-seen
	if first != want || second != first {
		t.Fatalf("bodies sent = %q then %q, want %q both times", first, second, want)
	}
}

// A body is sent without the newline that ends it, however many the file
// has: httpfmt trims a body's trailing blank lines and rr strips the one
// newline it leaves, so what goes on the wire ends where the text does.
func TestRunSendsBodyWithoutTrailingNewline(t *testing.T) {
	seen := make(chan string, 1)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- string(b)
	})

	path := write(t, srv, "-- x --\nPOST "+urlMark+"/ HTTP/1.1\nHost: h\n\n{\"a\":1}\n\n\n")
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got, want := <-seen, `{"a":1}`; got != want {
		t.Fatalf("body sent = %q, want %q", got, want)
	}
}

// A file reached through a symbolic link is written through it: the link
// survives, and its target holds the new contents.
func TestFormatFileThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.rr")
	link := filepath.Join(dir, "link.rr")
	if err := os.WriteFile(target, []byte("-- x --\nget https://example.com/ HTTP/1.1\nhost: h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := formatFile(link); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	want := "-- x --\nGET https://example.com/ HTTP/1.1\nHost: h\n\n"
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("target = %q, want %q", data, want)
	}
}

// The request carries the context, so cancelling it - which is what ^C does,
// through signal.NotifyContext - drops a request in flight rather than
// waiting out the client timeout. Nothing answered, so the file is untouched.
func TestRunCancel(t *testing.T) {
	started := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // answer nothing until the client gives up
	})

	file := "-- x --\nGET " + urlMark + "/ HTTP/1.1\nHost: h\n\n"
	path := write(t, srv, file)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- run(ctx, path, option{client: testClient(srv), out: io.Discard}) }()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the context was cancelled")
	}
	if got := read(t, srv, path); got != file {
		t.Errorf("file changed on cancel:\n%s\nwant:\n%s", got, file)
	}
}

// Field lines that share a name are all sent, in the order the file has them
// (RFC 9110, 5.3), rather than collapsed to one.
func TestRunSendsRepeatedFields(t *testing.T) {
	got := make(chan []string, 1)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header["X-Tag"]
	})

	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\nHost: h\nX-Tag: 1\nX-Tag: 2\n\n")
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if tags := <-got; !slices.Equal(tags, []string{"1", "2"}) {
		t.Fatalf("server saw X-Tag = %q, want [1 2]", tags)
	}
}

func TestRunDirectory(t *testing.T) {
	dir := t.TempDir()
	err := run(t.Context(), dir, option{client: http.DefaultClient, out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v, want directory error", err)
	}
}

// A variable is expanded on the way out, never on disk: the file stays safe
// to commit.
func TestRunKeepsVariablesOnDisk(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok")
	})
	t.Setenv("TOKEN", "secret")

	req := `GET {{url}}/ HTTP/1.1
Host: example.com
Authorization: Bearer ${TOKEN}

`
	path := write(t, srv, "-- x --\n"+req)
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	if got := string(sections(t, read(t, srv, path))[0].req); got != req {
		t.Errorf("request half:\n%s\n\nwant:\n%s", got, req)
	}
}

func TestRunKeepsMode(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\nHost: example.com\n\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %v, want %v", got, os.FileMode(0o640))
	}
}

// The files above are folded to LF for readability; a response is really
// stored in wire format, with CRLF.
func TestStoredResponseIsWireFormat(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\nHost: example.com\n\n")
	if err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resp := string(sections(t, string(data))[0].resp)
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("response half is not CRLF:\n%q", resp)
	}
}

// exch is an exchange as a test writes one: splitFile's three fields, as the
// text they are read from rather than the bytes they are kept in.
type exch struct{ name, req, resp string }

func TestSplitFile(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		head    string
		want    []exch
		wantErr string
	}{
		{
			name: "one exchange",
			in:   "-- a --\nGET https://x/\n\n",
			want: []exch{{"a", "GET https://x/\n\n", ""}},
		},
		{
			name: "a comment above the first marker",
			in:   "notes\n\n-- a --\nGET https://x/\n",
			head: "notes\n\n",
			want: []exch{{"a", "GET https://x/\n", ""}},
		},
		{
			name: "a stored response",
			in:   "-- a --\nGET https://x/\n\nHTTP/1.1 200 OK\n\nhi\n",
			want: []exch{{"a", "GET https://x/\n\n", "HTTP/1.1 200 OK\n\nhi\n"}},
		},
		{
			name: "two exchanges",
			in:   "-- a --\nGET https://x/\n-- b --\nGET https://y/\n",
			want: []exch{{"a", "GET https://x/\n", ""}, {"b", "GET https://y/\n", ""}},
		},
		{
			name: "a CRLF file",
			in:   "-- a --\r\nGET https://x/\r\n\r\nHTTP/1.1 200 OK\r\n",
			want: []exch{{"a", "GET https://x/\r\n\r\n", "HTTP/1.1 200 OK\r\n"}},
		},
		{
			// The old delimiter is four bytes, too short to be a marker, so
			// it is a line of the request like any other.
			name: "the old delimiter is text",
			in:   "-- a --\nGET https://x/\n----\nHTTP/1.1 200 OK\n",
			want: []exch{{"a", "GET https://x/\n----\n", "HTTP/1.1 200 OK\n"}},
		},
		{
			name: "a marker mid-line is text",
			in:   "-- a --\nGET https://x/-- b --\n",
			want: []exch{{"a", "GET https://x/-- b --\n", ""}},
		},
		{
			// The response is found at the version and the code, so a header
			// merely naming HTTP does not begin one.
			name: "a header that is not a status line",
			in:   "-- a --\nGET https://x/\nX-Note: HTTP/1.1 is fine\n\n",
			want: []exch{{"a", "GET https://x/\nX-Note: HTTP/1.1 is fine\n\n", ""}},
		},
		{
			name: "a response over HTTP/2",
			in:   "-- a --\nGET https://x/\n\nHTTP/2.0 204 No Content\n",
			want: []exch{{"a", "GET https://x/\n\n", "HTTP/2.0 204 No Content\n"}},
		},
		{name: "empty", in: "", wantErr: "no exchange"},
		{name: "no marker at all", in: "GET https://x/\n", wantErr: "no exchange"},
		{name: "no name", in: "--  --\nGET https://x/\n", wantErr: "no name"},
		{
			name:    "two of one name",
			in:      "-- a --\nGET https://x/\n-- a --\nGET https://y/\n",
			wantErr: "two exchanges named a",
		},
		{
			name:    "no request",
			in:      "-- a --\nHTTP/1.1 200 OK\n",
			wantErr: "exchange a has no request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, secs, err := splitFile([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(head) != tt.head {
				t.Errorf("head = %q, want %q", head, tt.head)
			}
			if len(secs) != len(tt.want) {
				t.Fatalf("got %d exchanges, want %d", len(secs), len(tt.want))
			}
			for i, s := range secs {
				w := tt.want[i]
				if s.name != w.name || string(s.req) != w.req || string(s.resp) != w.resp {
					t.Errorf("exchange %d = %q, %q, %q\nwant %q, %q, %q",
						i, s.name, s.req, s.resp, w.name, w.req, w.resp)
				}
			}
		})
	}
}

// What splitFile reads, rewriteFile writes: a file rr has already written
// comes back the same.
func TestSplitFileRoundTrip(t *testing.T) {
	want := "notes\n\n-- a --\nGET https://x/ HTTP/1.1\n\nHTTP/1.1 200 OK\n\nhi\n-- b --\nGET https://y/ HTTP/1.1\n\n"
	path := write(t, nil, want)
	data, mode, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, secs, err := splitFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteFile(path, mode, head, secs); err != nil {
		t.Fatal(err)
	}
	if got := read(t, nil, path); got != want {
		t.Errorf("round trip:\n%q\nwant:\n%q", got, want)
	}
}

func TestMarkerName(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"-- a --", "a", true},
		{"-- github/repos/list --", "github/repos/list", true},
		{"--   a   --", "a", true},
		{"--  --", "", true}, // a marker, but one splitFile refuses
		{"-- --", "", false}, // five bytes: no room for a name
		{"----", "", false},
		{"--------", "", false}, // a run of dashes holds no space to open one
		{"-- a", "", false},
		{"a --", "", false},
		{" -- a --", "", false},
		{"-- a -- ", "", false},
		{"GET https://x/-- a --", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			name, ok := markerName([]byte(tt.in))
			if name != tt.want || ok != tt.ok {
				t.Fatalf("markerName(%q) = %q, %v, want %q, %v", tt.in, name, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIsStatus(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"HTTP/1.1 200 OK", true},
		{"HTTP/1.0 404 Not Found", true},
		{"HTTP/2.0 204 No Content", true},
		{"HTTP/1.1 200", true},
		{"HTTP/1.1 20 OK", false},
		{"HTTP/1.1 2000 OK", false},
		{"HTTP/1.1 abc OK", false},
		{"HTTP/x.y 200 OK", false},
		{"HTTP/11 200 OK", false},
		{"HTTP/1.1", false},
		{"X-Note: HTTP/1.1 200 OK", false},
		{" HTTP/1.1 200 OK", false},
		{"GET https://x/ HTTP/1.1", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := isStatus([]byte(tt.in)); got != tt.want {
				t.Fatalf("isStatus(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPick(t *testing.T) {
	secs := []section{{name: "repos/list"}, {name: "users/list"}, {name: "repos/create"}}
	tests := []struct {
		name string
		re   string
		want []int
	}{
		{"no pattern takes every one", "", []int{0, 1, 2}},
		{"anchored", "^repos/", []int{0, 2}},
		{"unanchored", "list", []int{0, 1}},
		{"nothing", "^nope", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var re *regexp.Regexp
			if tt.re != "" {
				re = regexp.MustCompile(tt.re)
			}
			if got := pick(secs, re); !slices.Equal(got, tt.want) {
				t.Fatalf("pick(%q) = %v, want %v", tt.re, got, tt.want)
			}
		})
	}
}

func TestPattern(t *testing.T) {
	empty := ""
	bad := "("
	good := "^a"

	if re, err := pattern(nil); re != nil || err != nil {
		t.Errorf("pattern(nil) = %v, %v, want nil, nil", re, err)
	}
	if re, err := pattern(&empty); re != nil || err != nil {
		t.Errorf("pattern(%q) = %v, %v, want nil, nil", empty, re, err)
	}
	if _, err := pattern(&bad); err == nil {
		t.Errorf("pattern(%q) = nil error, want one", bad)
	}
	re, err := pattern(&good)
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("ab") || re.MatchString("ba") {
		t.Errorf("pattern(%q) does not match as written", good)
	}
}

// A nil re, which is what an unwritten -omit leaves, drops nothing. Naming
// Connection clears the field [http.Response.Write] writes that header from,
// there being no use in deleting a header the writer puts back.
func TestOmitHeaders(t *testing.T) {
	answer := func() *http.Response {
		return &http.Response{
			Close: true,
			Header: http.Header{
				"Connection":       {"close"},
				"Content-Type":     {"text/plain"},
				"Date":             {"Mon, 01 Jan 2035 00:00:00 GMT"},
				"X-Amz-Request-Id": {"7f3c"},
				"X-Request-Id":     {"abcd"},
			},
		}
	}
	tests := []struct {
		re    *regexp.Regexp
		want  []string
		close bool
	}{
		{nil, []string{"Connection", "Content-Type", "Date", "X-Amz-Request-Id", "X-Request-Id"}, true},
		{regexp.MustCompile("^(Date|X-Amz-)"), []string{"Connection", "Content-Type", "X-Request-Id"}, true},
		{regexp.MustCompile("Request-Id$"), []string{"Connection", "Content-Type", "Date"}, true},
		{regexp.MustCompile("^Connection$"), []string{"Content-Type", "Date", "X-Amz-Request-Id", "X-Request-Id"}, false},
		{regexp.MustCompile(".*"), nil, false},
	}
	for _, tt := range tests {
		resp := answer()
		omitHeaders(resp, tt.re)
		if got := slices.Sorted(maps.Keys(resp.Header)); !slices.Equal(got, tt.want) {
			t.Errorf("omitHeaders(%v) leaves %v, want %v", tt.re, got, tt.want)
		}
		if resp.Close != tt.close {
			t.Errorf("omitHeaders(%v) leaves Close = %v, want %v", tt.re, resp.Close, tt.close)
		}
	}
}
func TestExpandEnv(t *testing.T) {
	t.Setenv("NAME", "value")
	t.Setenv("DOLLAR", "a$NAME b${NAME}")
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"none", "no variables here", "no variables here", ""},
		{"brace", "x ${NAME} y", "x value y", ""},
		{"plain", "x $NAME y", "x value y", ""},
		{"adjacent", "$NAME${NAME}", "valuevalue", ""},
		// A substituted value is data, not a template: never rescan it.
		{"no reexpansion", "${DOLLAR}", "a$NAME b${NAME}", ""},
		{"digit is not a name", `{"price":"$5"}`, `{"price":"$5"}`, ""},
		{"bare dollars", "$$ $ ${}", "$$ $ ${}", ""},
		{"unset", "${A} $B", "", "unset: A, B"},
		{"partly unset", "${NAME} ${GONE}", "", "unset: GONE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandEnv([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGen(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{
			name: "a method and a URL alone",
			file: "-- items --\nget https://example.com/items HTTP/1.1\n",
			want: "# items\ncurl -X GET https://example.com/items\n",
		},
		{
			// Names come out canonical and sorted, since a header map keeps
			// neither the spelling nor the order the file had, and the body
			// comes out as httpfmt indented it.
			name: "headers and a JSON body",
			file: `-- items/create --
post https://example.com/items HTTP/1.1
content-type: application/json
x-note: it's fine
accept: a
accept: b
x-empty:

{"name": "x"}
`,
			want: `# items/create
curl -X POST https://example.com/items \
  -H 'Accept: a' \
  -H 'Accept: b' \
  -H 'Content-Type: application/json' \
  -H 'X-Empty;' \
  -H 'X-Note: it'\''s fine' \
  --data-raw '{
  "name": "x"
}'
`,
		},
		{
			name: "a stored response is no part of the request",
			file: `-- x --
GET https://example.com/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 2

hi
`,
			want: "# x\ncurl -X GET https://example.com/\n",
		},
		{
			// One command an exchange, named above it: the comment survives a
			// pipe to a shell and says which request it is.
			name: "every exchange in the file",
			file: "-- a --\nGET https://example.com/a HTTP/1.1\n-- b --\nGET https://example.com/b HTTP/1.1\n",
			want: "# a\ncurl -X GET https://example.com/a\n\n# b\ncurl -X GET https://example.com/b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, nil, tt.file)

			var b bytes.Buffer
			if err := gen(t.Context(), "curl", path, nil, &b); err != nil {
				t.Fatal(err)
			}

			if got := b.String(); got != tt.want {
				t.Fatalf("gen:\n%s\nwant:\n%s", got, tt.want)
			}
			if got := read(t, nil, path); got != tt.file {
				t.Errorf("file changed:\n%s\n\nwant:\n%s", got, tt.file)
			}
		})
	}
}

func TestGenMatch(t *testing.T) {
	file := "-- repos/list --\nGET https://example.com/a HTTP/1.1\n-- users/list --\nGET https://example.com/b HTTP/1.1\n"
	path := write(t, nil, file)

	var b bytes.Buffer
	if err := gen(t.Context(), "curl", path, regexp.MustCompile("^users/"), &b); err != nil {
		t.Fatal(err)
	}
	want := "# users/list\ncurl -X GET https://example.com/b\n"
	if got := b.String(); got != want {
		t.Fatalf("gen:\n%s\nwant:\n%s", got, want)
	}
}

func TestGenNoMatch(t *testing.T) {
	path := write(t, nil, "-- a --\nGET https://example.com/ HTTP/1.1\n")

	var b bytes.Buffer
	err := gen(t.Context(), "curl", path, regexp.MustCompile("nope"), &b)
	if err == nil || !strings.Contains(err.Error(), "no exchange matches") {
		t.Fatalf("err = %v, want no exchange matches", err)
	}
	if b.Len() != 0 {
		t.Errorf("wrote %q, want nothing", b.String())
	}
}

func TestGenExpands(t *testing.T) {
	// The command carries the value and the file keeps the variable: what is
	// written is what run would send, and the file stays safe to commit.
	t.Setenv("TOKEN", "s3cret")
	file := `-- x --
GET https://example.com/ HTTP/1.1
Authorization: Bearer ${TOKEN}

`
	path := write(t, nil, file)

	var b bytes.Buffer
	if err := gen(t.Context(), "curl", path, nil, &b); err != nil {
		t.Fatal(err)
	}

	want := `# x
curl -X GET https://example.com/ \
  -H 'Authorization: Bearer s3cret'
`
	if got := b.String(); got != want {
		t.Fatalf("gen:\n%s\nwant:\n%s", got, want)
	}
	if got := read(t, nil, path); got != file {
		t.Errorf("file changed:\n%s\n\nwant:\n%s", got, file)
	}
}

func TestGenUnknownFormComesFirst(t *testing.T) {
	// The form is checked before the file is opened, so a typo is reported
	// as itself rather than as trouble with a file that was never at fault.
	err := gen(t.Context(), "httpie", filepath.Join(t.TempDir(), "nope.rr"), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown form: httpie") {
		t.Fatalf("err = %v, want it to name the form", err)
	}
}

func TestGenErrors(t *testing.T) {
	tests := []struct {
		name string
		form string
		file string
		want string
		// A form rr does not know is no fault of the file, so it alone is
		// not an *fs.PathError.
		pathErr bool
	}{
		{
			name: "a form rr does not know",
			form: "httpie",
			file: "-- x --\nGET https://example.com/ HTTP/1.1\n\n",
			want: "unknown form: httpie",
		},
		{
			name:    "every unset variable is named",
			form:    "curl",
			file:    "-- x --\nGET https://example.com/$ALSO HTTP/1.1\nAuthorization: Bearer ${MISSING}\n\n",
			want:    "unset: ALSO, MISSING",
			pathErr: true,
		},
		{
			name:    "the exchange is named",
			form:    "curl",
			file:    "-- items/create --\nGET /path HTTP/1.1\nHost: example.com\n\n",
			want:    "items/create: ",
			pathErr: true,
		},
		{
			name:    "request URI is not absolute",
			form:    "curl",
			file:    "-- x --\nGET /path HTTP/1.1\nHost: example.com\n\n",
			want:    "absolute",
			pathErr: true,
		},
		{
			name:    "an empty file",
			form:    "curl",
			file:    "",
			want:    "no exchange",
			pathErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, nil, tt.file)

			var b bytes.Buffer
			err := gen(t.Context(), tt.form, path, nil, &b)
			if err == nil {
				t.Fatalf("gen wrote %q, want an error", b.String())
			}

			var perr *fs.PathError
			if got := errors.As(err, &perr); got != tt.pathErr {
				t.Fatalf("err = %v; *fs.PathError is %v, want %v", err, got, tt.pathErr)
			}
			msg := err.Error()
			if tt.pathErr {
				if perr.Path != path {
					t.Errorf("err names %s, want %s", perr.Path, path)
				}
				// Matching the inner error keeps the temporary path, which
				// embeds this subtest's name, from matching in its place.
				msg = perr.Err.Error()
			}
			if !strings.Contains(msg, tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if b.Len() != 0 {
				t.Errorf("wrote %q on error, want nothing", b.String())
			}
		})
	}
}
