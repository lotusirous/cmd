package main

import (
	"bytes"
	"context"
	"errors"
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

// parsed parses text the way rr does, so a test can look at one half of one
// exchange without cutting the text itself.
func parsed(t *testing.T, text string) []exchange {
	t.Helper()
	_, exchanges, err := parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return exchanges
}

// stdoutFor returns what run should print for the exchanges in want, which is
// each response under the marker naming it.
func stdoutFor(t *testing.T, want string) string {
	t.Helper()
	var b strings.Builder
	for _, s := range parsed(t, want) {
		if len(s.resp) > 0 {
			b.WriteString(markerLine(s.name))
			b.Write(s.resp)
		}
	}
	return b.String()
}

// Rr fmt writes the file in place, so the mode it was made with is the mode
// it keeps. What it writes is testdata's to say.
func TestFormatFileKeepsMode(t *testing.T) {
	path := write(t, nil, "-- x --\nget https://example.com/ HTTP/1.1\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := formatFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %v, want %v", got, os.FileMode(0o640))
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

// A failed write to standard output is not a failed exchange. The answer
// arrived, and the file is where rr says so; only the saying of it failed,
// which is what a pipe to a program that stops reading does.
func TestRunStoresWhenReportFails(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})

	want := `-- x --
GET {{url}}/ HTTP/1.1

HTTP/1.1 200 OK
Content-Length: 2
Content-Type: text/plain

ok
`
	path := write(t, srv, "-- x --\nGET "+urlMark+"/ HTTP/1.1\n\n")
	err := run(t.Context(), path, option{client: testClient(srv), out: brokenPipe{}})
	if !errors.Is(err, errBrokenPipe) {
		t.Fatalf("err = %v, want %v", err, errBrokenPipe)
	}
	if got := read(t, srv, path); got != want {
		t.Errorf("file after run:\n%s\n\nwant:\n%s", got, want)
	}
}

var errBrokenPipe = errors.New("broken pipe")

// brokenPipe is standard output that has stopped being read.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errBrokenPipe }

// A run can fail twice over: the exchange that stopped it says why, and a
// file that cannot be written says the run went unrecorded. Ranking them
// would drop the second, leaving rr to claim a file it never wrote.
func TestRunReportsWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})

	path := write(t, srv, `-- a --
GET {{url}}/ HTTP/1.1

-- b --
GET http://127.0.0.1:1/nope HTTP/1.1

`)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), path, option{client: testClient(srv), out: io.Discard})
	if err == nil {
		t.Fatal("err = nil, want the exchange that failed and the file that could not be written")
	}
	if !strings.Contains(err.Error(), "send") {
		t.Errorf("err = %v, want it to name the exchange that stopped the run", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want it to report the file it could not write", err)
	}
}

// -timeout gives an exchange a deadline of its own. What never answered is
// not stored, so the file is left as it was.
func TestRunTimesOut(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // answer nothing until the client gives up
	})

	file := "-- x --\nGET " + urlMark + "/ HTTP/1.1\n\n"
	path := write(t, srv, file)
	client := testClient(srv)
	client.Timeout = 50 * time.Millisecond

	err := run(t.Context(), path, option{client: client, out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "x:") {
		t.Fatalf("err = %v, want it to name the exchange x", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline exceeded under it", err)
	}
	if got := read(t, srv, path); got != file {
		t.Errorf("file changed:\n%s\n\nwant:\n%s", got, file)
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
	resp := string(parsed(t, string(data))[0].resp)
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("response half is not CRLF:\n%q", resp)
	}
}

func TestPick(t *testing.T) {
	exchanges := []exchange{{name: "repos/list"}, {name: "users/list"}, {name: "repos/create"}}
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
			if got := pick(exchanges, re); !slices.Equal(got, tt.want) {
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
