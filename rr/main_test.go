package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestRun checks the whole round trip: file in, file out. Each want is the
// complete file rr should leave behind, and stdout must be its response half.
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
			file: `GET {{url}}/ HTTP/1.1
Host: example.com
X-Test: yes

`,
			want: `GET {{url}}/ HTTP/1.1
Host: example.com
X-Test: yes

----
HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello`,
		},
		{
			name: "json body is indented",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true,"n":1}`)
			},
			file: `POST {{url}}/ HTTP/1.1
Host: example.com
Content-Length: 4

body`,
			want: `POST {{url}}/ HTTP/1.1
Host: example.com

body
----
HTTP/1.1 200 OK
Content-Length: 26
Content-Type: application/json

{
  "ok": true,
  "n": 1
}`,
		},
		{
			name: "an old response is replaced",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "v2")
			},
			file: `GET {{url}}/ HTTP/1.1
Host: example.com

----
HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

v1`,
			want: `GET {{url}}/ HTTP/1.1
Host: example.com

----
HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

v2`,
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
			file: `GET {{url}}/ HTTP/1.1
Host: example.com

`,
			want: `GET {{url}}/ HTTP/1.1
Host: example.com

----
HTTP/1.1 200 OK
Content-Length: 7
Content-Type: text/plain

chunked`,
		},
		{
			// A hand-written file often stops after the last header. The
			// header block is unterminated, but unambiguous: rr completes it
			// and stores the file in canonical form.
			name: "a file that ends after the last header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			},
			file: `GET {{url}}/ HTTP/1.1
Host: example.com`,
			want: `GET {{url}}/ HTTP/1.1
Host: example.com

----
HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello`,
		},
		{
			// http.ReadRequest would drop this body for want of framing.
			name:    "a body needs no Content-Length",
			handler: echoBody,
			file: `POST {{url}}/ HTTP/1.1
Host: example.com
Content-Type: application/json

{"a":1}`,
			want: `POST {{url}}/ HTTP/1.1
Host: example.com
Content-Type: application/json

{
  "a": 1
}
----
HTTP/1.1 200 OK
Content-Length: 16
Content-Type: text/plain

got {
  "a": 1
}`,
		},
		{
			// ... and would send only the first two bytes of it here.
			name:    "a stale Content-Length is dropped",
			handler: echoBody,
			file: `POST {{url}}/ HTTP/1.1
Host: example.com
Content-Length: 2

{"a":1}`,
			want: `POST {{url}}/ HTTP/1.1
Host: example.com

{"a":1}
----
HTTP/1.1 200 OK
Content-Length: 11
Content-Type: text/plain

got {"a":1}`,
		},
		{
			// The body parses as JSON, but text/plain says it is not one.
			name: "a response is indented only when it says it is json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, `{"a":1}`)
			},
			file: `GET {{url}}/ HTTP/1.1
Host: example.com

`,
			want: `GET {{url}}/ HTTP/1.1
Host: example.com

----
HTTP/1.1 200 OK
Content-Length: 7
Content-Type: text/plain

{"a":1}`,
		},
		{
			// An absolute request URI carries the host, so Host is optional.
			name: "an absolute URI needs no Host header",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "hello")
			},
			file: `GET {{url}}/ HTTP/1.1

`,
			want: `GET {{url}}/ HTTP/1.1

----
HTTP/1.1 200 OK
Content-Length: 5
Content-Type: text/plain

hello`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(t, tt.handler)
			path := write(t, srv, tt.file)

			var stdout bytes.Buffer
			if err := run(t.Context(), path, testClient(srv), &stdout); err != nil {
				t.Fatal(err)
			}

			if got := read(t, srv, path); got != tt.want {
				t.Errorf("file after run:\n%s\n\nwant:\n%s", got, tt.want)
			}
			_, wantStdout, _ := strings.Cut(tt.want, delim+"\n")
			if got := canon(srv, stdout.String()); got != wantStdout {
				t.Errorf("stdout:\n%s\n\nwant:\n%s", got, wantStdout)
			}
		})
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
			file: `GET /path HTTP/1.1
Host: example.com

`,
			want: "absolute",
		},
		{
			name: "every unset variable is named",
			file: `GET https://example.com/$ALSO HTTP/1.1
Host: example.com
Authorization: Bearer ${MISSING}

`,
			want: "unset: ALSO, MISSING",
		},
		{
			name: "the server cannot be reached",
			file: `GET http://127.0.0.1:1/nope HTTP/1.1
Host: 127.0.0.1:1

----
HTTP/1.1 500 Old
Content-Length: 3

old`,
			want: "127.0.0.1:1",
		},
		{
			// RFC 9112, 3.2: a request with two Host lines is malformed, and
			// http.ReadRequest refuses it rather than pick one.
			name: "two Host headers",
			file: `GET https://example.com/ HTTP/1.1
Host: one
Host: two

`,
			want: "too many Host headers",
		},
		{
			name: "an empty file",
			file: "",
			want: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, nil, tt.file)

			err := run(t.Context(), path, &http.Client{CheckRedirect: noRedirect}, io.Discard)

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
// client, and whatever follows ---- is left byte for byte.
func TestFormatFile(t *testing.T) {
	before := `get https://example.com/ HTTP/1.1
content-type:application/json
content-length: 2

{"a":1}
` + delim + `
HTTP/1.1 200 OK
Content-Length: 2

hi`
	after := `GET https://example.com/ HTTP/1.1
Content-Type: application/json

{
  "a": 1
}
` + delim + `
HTTP/1.1 200 OK
Content-Length: 2

hi`

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

// A file with no response yet gains no ---- line.
func TestFormatFileNoResponse(t *testing.T) {
	path := write(t, nil, "get https://example.com/ HTTP/1.1\nhost: h")
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	want := "GET https://example.com/ HTTP/1.1\nHost: h\n\n"
	if got := read(t, nil, path); got != want {
		t.Errorf("formatted:\n%q\nwant:\n%q", got, want)
	}
}

// Running a file formats it, and leaves the mode alone.
func TestRunFormats(t *testing.T) {
	srv := serve(t, echoBody)
	before := `post {{url}}/ HTTP/1.1
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
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}

	req, _, _ := strings.Cut(read(t, srv, path), delim+"\n")
	if req != after {
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
	path := write(t, srv, "POST "+urlMark+"/ HTTP/1.1\nHost: h\n\n{\"a\":1}")
	for i := range 2 {
		if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	want := `{"a":1}`
	first, second := <-seen, <-seen
	if first != want || second != first {
		t.Fatalf("bodies sent = %q then %q, want %q both times", first, second, want)
	}
}

// A file reached through a symbolic link is written through it: the link
// survives, and its target holds the new contents.
func TestFormatFileThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.rr")
	link := filepath.Join(dir, "link.rr")
	if err := os.WriteFile(target, []byte("get https://example.com/ HTTP/1.1\nhost: h"), 0o644); err != nil {
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
	want := "GET https://example.com/ HTTP/1.1\nHost: h\n\n"
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
// waiting out the client timeout.
func TestRunCancel(t *testing.T) {
	started := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // answer nothing until the client gives up
	})

	file := "GET " + urlMark + "/ HTTP/1.1\nHost: h\n\n"
	path := write(t, srv, file)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- run(ctx, path, testClient(srv), io.Discard) }()
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

	path := write(t, srv, "GET "+urlMark+"/ HTTP/1.1\nHost: h\nX-Tag: 1\nX-Tag: 2\n\n")
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}
	if tags := <-got; !slices.Equal(tags, []string{"1", "2"}) {
		t.Fatalf("server saw X-Tag = %q, want [1 2]", tags)
	}
}

func TestRunDirectory(t *testing.T) {
	dir := t.TempDir()
	err := run(t.Context(), dir, http.DefaultClient, io.Discard)
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

	file := `GET {{url}}/ HTTP/1.1
Host: example.com
Authorization: Bearer ${TOKEN}

`
	path := write(t, srv, file)
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}

	req, _, _ := strings.Cut(read(t, srv, path), delim+"\n")
	if req != file {
		t.Errorf("request half:\n%s\n\nwant:\n%s", req, file)
	}
}

func TestRunKeepsMode(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	path := write(t, srv, "GET "+urlMark+"/ HTTP/1.1\nHost: example.com\n\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
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

// The files above are folded to LF for readability; the response half is
// really stored in wire format, with CRLF.
func TestStoredResponseIsWireFormat(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	path := write(t, srv, "GET "+urlMark+"/ HTTP/1.1\nHost: example.com\n\n")
	if err := run(t.Context(), path, testClient(srv), io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := strings.Cut(string(data), delim+"\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("response half is not CRLF:\n%q", resp)
	}
}

func TestSplitFile(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantReq  string
		wantRest string
	}{
		{"empty", "", "", ""},
		{"request only", "GET https://x/\n\n", "GET https://x/\n\n", ""},
		{"with response", "GET https://x/\n\n" + delim + "\nHTTP/1.1 200\n\n", "GET https://x/\n\n", delim + "\nHTTP/1.1 200\n\n"},
		{"cr delimiter", "GET https://x/\r\n" + delim + "\r\nHTTP/1.1 200\r\n", "GET https://x/\r\n", delim + "\r\nHTTP/1.1 200\r\n"},
		{"delimiter first", delim + "\nHTTP/1.1 200\n", "", delim + "\nHTTP/1.1 200\n"},
		{"delimiter unterminated", "GET https://x/\n" + delim, "GET https://x/\n", delim},
		{"delimiter mid-line", "GET https://x/----\n", "GET https://x/----\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, rest := splitFile([]byte(tt.in))
			if string(req) != tt.wantReq || string(rest) != tt.wantRest {
				t.Fatalf("req = %q, rest = %q\nwant %q, %q", req, rest, tt.wantReq, tt.wantRest)
			}
		})
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
