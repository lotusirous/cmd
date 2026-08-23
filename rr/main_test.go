package main

// What a case file cannot say: a report that will not write, a file that will
// not, a run that is stopped, a symlink. Everything else is in testdata.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, srv *httptest.Server, content string) string {
	t.Helper()
	if srv != nil {
		content = strings.ReplaceAll(content, urlMark, srv.URL)
	}
	path := filepath.Join(t.TempDir(), "f.rr")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func held(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), srv.URL, urlMark)
}

var errBrokenPipe = errors.New("broken pipe")

type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errBrokenPipe }

// The file is what the run is for; stdout only says so.
func TestRunStoresWhenReportFails(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})
	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\n\n")

	_, err := runFile(t.Context(), brokenPipe{}, path, "", option{client: testClient(srv)})
	if !errors.Is(err, errBrokenPipe) {
		t.Fatalf("err = %v, want %v", err, errBrokenPipe)
	}
	want := "-- x --\nGET {{url}}/ HTTP/1.1\n\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: text/plain\r\n\r\nok\n"
	if got := held(t, srv, path); got != want {
		t.Errorf("file after run:\n%q\nwant:\n%q", got, want)
	}
}

func TestRunReportsWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	path := write(t, srv, "-- a --\nGET "+urlMark+"/ HTTP/1.1\n\n-- b --\nGET http://127.0.0.1:1/nope HTTP/1.1\n\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	_, err := runFile(t.Context(), io.Discard, path, "", option{client: testClient(srv)})
	if err == nil || !strings.Contains(err.Error(), "send") {
		t.Fatalf("err = %v, want it to name the exchange that stopped the run", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want it to report the file it could not write", err)
	}
}

// However a run is stopped, the file is left as it was written.
func TestRunStopsOnTimeout(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // answer nothing until the client gives up
	})
	file := "-- x --\nGET " + urlMark + "/ HTTP/1.1\n\n"
	path := write(t, srv, file)
	client := testClient(srv)
	client.Timeout = 50 * time.Millisecond

	_, err := runFile(t.Context(), io.Discard, path, "", option{client: client})
	if err == nil || !strings.Contains(err.Error(), "x:") {
		t.Fatalf("err = %v, want it to name the exchange x", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline exceeded under it", err)
	}
	if got := held(t, srv, path); got != file {
		t.Errorf("file changed:\n%s\nwant:\n%s", got, file)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		cancel() // the caller gives up while this one is in flight
		<-r.Context().Done()
	})
	file := "-- x --\nGET " + urlMark + "/ HTTP/1.1\n\n"
	path := write(t, srv, file)

	_, err := runFile(ctx, io.Discard, path, "", option{client: testClient(srv)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := held(t, srv, path); got != file {
		t.Errorf("file changed on cancel:\n%s\nwant:\n%s", got, file)
	}
}

func TestFormatThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target.rr"), filepath.Join(dir, "link.rr")
	if err := os.WriteFile(target, []byte("-- x --\nget https://example.com/ HTTP/1.1\nhost: h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := format(io.Discard, []string{link}, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	data, err := os.ReadFile(target)
	want := "-- x --\nGET https://example.com/ HTTP/1.1\nHost: h\n\n"
	if err != nil || string(data) != want {
		t.Errorf("target = %q, %v, want %q", data, err, want)
	}
}

// A link met on the way down is stepped over: it would send the walk round
// again. One named outright is followed.
func TestFilesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "a.rr")
	for _, path := range []string{target, filepath.Join(sub, "c.rr")} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link, dirLink := filepath.Join(sub, "link.rr"), filepath.Join(t.TempDir(), "link")
	if err := errors.Join(os.Symlink(target, link), os.Symlink(sub, dirLink)); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		arg  string
		want []string
	}{
		{sub, []string{filepath.Join(sub, "c.rr")}},         // walked onto: stepped over
		{link, []string{link}},                              // named: a file
		{dirLink, []string{filepath.Join(dirLink, "c.rr")}}, // named: a directory
	} {
		got, err := files([]string{tt.arg})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("files(%s) = %q, want %q", tt.arg, got, tt.want)
		}
	}
}

func TestStoredResponseIsWireFormat(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	path := write(t, srv, "-- x --\r\nGET "+urlMark+"/ HTTP/1.1\r\n\r\n")
	if _, err := runFile(t.Context(), io.Discard, path, "", option{client: testClient(srv)}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.Exchanges[0].Response); !strings.HasPrefix(got, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("the stored response is not CRLF:\n%q", got)
	}
}

func TestPattern(t *testing.T) {
	empty, bad, good := "", "(", "^a"
	for _, s := range []*string{nil, &empty} {
		if re, err := pattern(s); re != nil || err != nil {
			t.Errorf("pattern(%v) = %v, %v, want nil, nil", s, re, err)
		}
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

// Connection is cleared rather than deleted, so no pattern reaches it.
func TestOmitHeaders(t *testing.T) {
	for _, tt := range []struct {
		re    string
		want  []string
		close bool
	}{
		{"", []string{"Connection", "Date", "X-Amz-Id", "X-Request-Id"}, true},
		{"^(Date|X-Amz-)", []string{"Connection", "X-Request-Id"}, true},
		{"Request-Id$", []string{"Connection", "Date", "X-Amz-Id"}, true},
		{"^Connection$", []string{"Date", "X-Amz-Id", "X-Request-Id"}, false},
		{".*", nil, false},
	} {
		re, err := pattern(&tt.re)
		if err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{Close: true, Header: http.Header{
			"Connection":   {"close"},
			"Date":         {"Mon, 01 Jan 2035 00:00:00 GMT"},
			"X-Amz-Id":     {"7f3c"},
			"X-Request-Id": {"abcd"},
		}}
		omitHeaders(resp, re)
		if got := slices.Sorted(maps.Keys(resp.Header)); !slices.Equal(got, tt.want) {
			t.Errorf("omitHeaders(%q) leaves %v, want %v", tt.re, got, tt.want)
		}
		if resp.Close != tt.close {
			t.Errorf("omitHeaders(%q) leaves Close = %v, want %v", tt.re, resp.Close, tt.close)
		}
	}
}
