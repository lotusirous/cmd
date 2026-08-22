package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAll runs the cases in testdata, after the fashion of ivy's tests. A
// case is one file, and it is the file rr is told to work on: the '#' lines
// at the top say what rr is told to do, what follows them is the file as it
// is written, and what is indented by a tab is what rr should leave behind —
// the file itself for run and fmt, the standard output for gen.
//
// The replies come from the case: what its indented half stores under an
// exchange is what the server answers that exchange with, so a case says once
// what a file holds after a run rather than saying it again as a list of
// replies. A directive covers what a file does not keep, a header rr is meant
// to drop being the plain example.
func TestAll(t *testing.T) {
	files, err := filepath.Glob("testdata/*.rr")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no cases in testdata")
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".rr"), func(t *testing.T) {
			for _, c := range readCases(t, file) {
				t.Run(c.name, func(t *testing.T) { c.run(t) })
			}
		})
	}
}

// A testCase is one file of testdata: what rr is told, the file it is told it
// about, and the text it should leave behind.
type testCase struct {
	file    string   // the file the case is in, for an error to name
	line    int      // the line it begins on, for an error to name
	name    string   // its title, which is the first line of it
	cmd     string   // run, fmt or gen
	match   string   // -match
	omit    string   // -omit
	form    string   // -form
	wantErr string   // the command must fail, saying this
	reply   []string // header lines the server sends that the file does not keep
	expect  []string // header lines the request must carry, or the reply is 400
	env     []string // NAME=value set for the case, for ${NAME} to expand from
	echo    bool     // the reply is the request body, as a server that echoes
	chunked bool     // the reply is written without a length
	in      string   // the file as it is written
	want    string   // the file rr should leave, or what gen should write
}

// readCases reads the cases in one file. A case is the '#' lines that say
// what rr is told, the file as it is written, and, indented by a tab, what rr
// should leave behind; it runs until the indenting stops, where the next case
// begins. A case is named by the first exchange it opens, so the subtests of
// a file read as the markers do.
//
// A blank line belongs to the case it is in, so cases follow one another with
// nothing between them: the indenting is what separates them. A body line
// beginning with a tab would be read as want, which no case has, rr indenting
// JSON with spaces.
func readCases(t *testing.T, file string) []*testCase {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cases []*testCase
	var c *testCase
	line := 0
	for text := range strings.Lines(string(data)) {
		line++
		s := strings.TrimSuffix(text, "\n")
		title := strings.HasPrefix(s, "#")
		blank := strings.TrimSpace(s) == ""

		if !blank && !strings.HasPrefix(text, "\t") && (c == nil || c.want != "") {
			c = &testCase{file: file, line: line, cmd: "run", form: "curl"}
			cases = append(cases, c)
		}
		switch {
		case c == nil:
			t.Fatalf("%s:%d: a case begins where the indenting stops", file, line)
		case strings.HasPrefix(text, "\t"):
			c.want += text[1:]
		case title && c.in == "":
			c.directive(t, strings.TrimSpace(strings.TrimPrefix(s, "#")))
		case c.want != "" && blank:
			c.want += text // a blank line of the want, which has no tab to keep it
		case c.want != "":
			t.Fatalf("%s:%d: %q comes after the want", file, line, s)
		default:
			c.in += text
		}
	}
	for _, c := range cases {
		c.name = c.title()
	}
	return cases
}

// title names the case: the first exchange its file opens, which is what a
// marker line is for. A case about a file that opens none answers to the
// failure it is there for, and one with neither to the line it begins on.
func (c *testCase) title() string {
	for line := range strings.Lines(c.in) {
		if name, ok := markerName([]byte(strings.TrimRight(line, "\r\n"))); ok && name != "" {
			return name
		}
	}
	if c.wantErr != "" {
		return c.wantErr
	}
	return fmt.Sprintf("line %d", c.line)
}

// directive reads one '#' line: the command rr is given, with its flags, or a
// word for what a file cannot say. A flag value holds no spaces, there being
// no quoting here.
func (c *testCase) directive(t *testing.T, s string) {
	t.Helper()
	verb, rest, _ := strings.Cut(s, " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "run", "fmt", "gen":
		c.cmd = verb
		args := strings.Fields(rest)
		for i := 0; i < len(args); i += 2 {
			if i+1 == len(args) {
				t.Fatalf("%s: %s has no value", c.where(), args[i])
			}
			switch args[i] {
			case "-match":
				c.match = args[i+1]
			case "-omit":
				c.omit = args[i+1]
			case "-form":
				c.form = args[i+1]
			default:
				t.Fatalf("%s: unknown flag %s", c.where(), args[i])
			}
		}
	case "error":
		c.wantErr = rest
	case "reply":
		c.reply = append(c.reply, rest)
	case "expect":
		c.expect = append(c.expect, rest)
	case "env":
		c.env = append(c.env, rest)
	case "echo":
		c.echo = true
	case "chunked":
		c.chunked = true
	case "note": // prose about the case, for a reader
	case "":
	default:
		t.Fatalf("%s: unknown directive %q", c.where(), verb)
	}
}

// run works the case: it serves the replies the case stores, runs the command
// the case names, and holds rr to the text the case says it leaves.
func (c *testCase) run(t *testing.T) {
	t.Helper()
	for _, e := range c.env {
		name, value, _ := strings.Cut(e, "=")
		t.Setenv(name, value)
	}
	var srv *httptest.Server
	if c.cmd == "run" {
		srv = c.serve(t)
	}
	path := write(t, srv, c.in)

	var stdout bytes.Buffer
	var err error
	switch c.cmd {
	case "run":
		err = run(t.Context(), path, option{
			match:  c.pattern(t, c.match),
			omit:   c.pattern(t, c.omit),
			client: testClient(srv),
			out:    &stdout,
		})
	case "fmt":
		err = formatFile(path)
	case "gen":
		err = gen(t.Context(), c.form, path, c.pattern(t, c.match), &stdout)
	}
	switch {
	case c.wantErr != "" && c.cmd == "gen":
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
		}
	case c.wantErr != "":
		// Every error rr run and rr fmt return is an *fs.PathError naming the
		// file. Matching on the inner error keeps the temporary path, which
		// holds this case's name, from matching in place of the message.
		var perr *fs.PathError
		if !errors.As(err, &perr) {
			t.Fatalf("err = %v, want an *fs.PathError", err)
		}
		if perr.Path != path {
			t.Errorf("err names %s, want %s", perr.Path, path)
		}
		if !strings.Contains(perr.Err.Error(), c.wantErr) {
			t.Fatalf("err = %v, want it to mention %q", perr.Err, c.wantErr)
		}
	case err != nil:
		t.Fatal(err)
	}

	if c.cmd == "gen" {
		if got := canon(srv, stdout.String()); got != c.want {
			t.Errorf(c.where()+" gen writes:\n%s\n\nwant:\n%s", got, c.want)
		}
		if got := read(t, srv, path); got != c.in {
			t.Errorf(c.where()+" gen changed the file:\n%s\n\nwant:\n%s", got, c.in)
		}
		return
	}
	if got := read(t, srv, path); got != c.want {
		t.Errorf(c.where()+" file after %s:\n%s\n\nwant:\n%s", c.cmd, got, c.want)
	}
	// Formatting a formatted file changes nothing, so fmt in a loop is a fixed
	// point rather than a diff. httpfmt asserts as much of a request, on every
	// case it has; this is of the whole file, markers and all.
	if c.cmd == "fmt" {
		if err := formatFile(path); err != nil {
			t.Fatal(err)
		}
		if got := read(t, srv, path); got != c.want {
			t.Errorf(c.where()+" formatting twice differs:\n%s\n\nwant:\n%s", got, c.want)
		}
	}
	// What rr prints is what it stores. A case that fails part way stores
	// what answered above it, which is not all the want holds, so the two are
	// the same text only when the whole run went through.
	if c.cmd == "run" && c.wantErr == "" {
		if got, want := canon(srv, stdout.String()), stdoutFor(t, c.want); got != want {
			t.Errorf(c.where()+" stdout:\n%s\n\nwant:\n%s", got, want)
		}
	}
}

// serve answers each request with the response the case stores for it: the
// exchanges of want, in the order rr sends them. What the file does not keep
// is what the directives are for.
func (c *testCase) serve(t *testing.T) *httptest.Server {
	t.Helper()
	replies := c.replies(t)
	n := 0
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		for _, h := range c.expect {
			name, want, _ := strings.Cut(h, ":")
			name, want = strings.TrimSpace(name), strings.TrimSpace(want)
			got := r.Header.Get(name)
			if name == "Host" {
				got = r.Host
			}
			if got != want {
				http.Error(w, name+" is "+got, http.StatusBadRequest)
				return
			}
		}
		if c.echo {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write(body)
			return
		}
		if n >= len(replies) {
			http.Error(w, "the case stores no reply for this request", http.StatusInternalServerError)
			return
		}
		reply := replies[n]
		n++
		if r.Method != reply.method || r.URL.RequestURI() != reply.uri {
			http.Error(w, r.Method+" "+r.URL.RequestURI()+", want "+reply.method+" "+reply.uri, http.StatusBadRequest)
			return
		}
		for name, value := range reply.resp.Header {
			w.Header()[name] = value
		}
		for _, h := range c.reply {
			name, value, _ := strings.Cut(h, ":")
			w.Header().Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
		if c.chunked {
			w.Header().Del("Content-Length") // no length: the server frames it as it likes
		}
		w.WriteHeader(reply.resp.StatusCode)
		io.Copy(w, reply.resp.Body)
	})
}

// where names the case in an error, as a compiler names a line.
func (c *testCase) where() string {
	return fmt.Sprintf("%s:%d:", c.file, c.line)
}

// A reply is one answer the case has stored, and the request the case says
// asks for it: the server hands them out in order, so a request that arrives
// out of order, or asks for the wrong thing, is answered 400 and the case
// fails on the text it stores.
type reply struct {
	method string // as the exchange writes it
	uri    string // what follows {{url}} in the exchange's request line
	resp   *http.Response
}

// replies returns the answers the case stores, in the order rr sends the
// exchanges that hold them.
func (c *testCase) replies(t *testing.T) []reply {
	t.Helper()
	f, err := Parse([]byte(c.want))
	if err != nil {
		return nil // a case whose want is no file has no replies to give
	}
	var replies []reply
	for _, i := range pick(f.Exchanges, c.pattern(t, c.match)) {
		ex := f.Exchanges[i]
		if len(ex.resp) == 0 {
			continue
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(ex.resp)), nil)
		if err != nil {
			t.Fatalf("%s: %s stores no response a server could send: %v", c.where(), ex.name, err)
		}
		line, _, _ := strings.Cut(string(ex.req), "\n")
		method, rest, _ := strings.Cut(line, " ")
		url, _, _ := strings.Cut(rest, " ")
		replies = append(replies, reply{
			method: strings.ToUpper(method),
			uri:    strings.TrimPrefix(url, urlMark),
			resp:   resp,
		})
	}
	return replies
}

// pattern compiles one of the case's flags, the way rr compiles it.
func (c *testCase) pattern(t *testing.T, s string) *regexp.Regexp {
	t.Helper()
	re, err := pattern(&s)
	if err != nil {
		t.Fatalf("%s: %v", c.where(), err)
	}
	return re
}

// exch is an exchange as a test writes one: parse's three fields, as the
// text they are read from rather than the bytes they are kept in.
type exch struct{ name, req, resp string }

// text writes the comment, then each exchange under the marker that names
// it, ending the last line of any part that left one open: what follows is a
// marker, and a marker is a line of its own.
func TestText(t *testing.T) {
	f := File{
		Comment: []byte("notes\n\n"),
		Exchanges: []Exchange{
			{
				name: "a",
				req:  []byte("GET https://x/ HTTP/1.1\n\n"),
				resp: []byte("HTTP/1.1 200 OK\n\nhi\n"),
			},
			{name: "b", req: []byte("GET https://y/ HTTP/1.1")}, // no newline, no response
		},
	}
	want := "notes\n\n" +
		"-- a --\nGET https://x/ HTTP/1.1\n\nHTTP/1.1 200 OK\n\nhi\n" +
		"-- b --\nGET https://y/ HTTP/1.1\n"
	if got := string(text(&f)); got != want {
		t.Errorf("text:\n%q\nwant:\n%q", got, want)
	}
	if got := string(text(&File{})); got != "" {
		t.Errorf("text of nothing = %q, want it empty", got)
	}
}

// Format puts the request half of every exchange in canonical form and leaves
// the rest as it is. The File it is given comes back untouched: what rr fmt
// writes is one thing, and what a run stores is the file's own text.
func TestFormat(t *testing.T) {
	f := File{
		Comment: []byte("notes\n\n"),
		Exchanges: []Exchange{
			{name: "a", req: []byte("get https://x/ HTTP/1.1\nhost: h\n"), resp: []byte("HTTP/1.1 200 OK\n\nhi\n")},
		},
	}
	want := "notes\n\n-- a --\nGET https://x/ HTTP/1.1\nHost: h\n\nHTTP/1.1 200 OK\n\nhi\n"
	if got := string(Format(&f)); got != want {
		t.Errorf("Format:\n%q\nwant:\n%q", got, want)
	}
	if want := "get https://x/ HTTP/1.1\nhost: h\n"; string(f.Exchanges[0].req) != want {
		t.Errorf("Format rewrote the File it was given: req = %q, want %q", f.Exchanges[0].req, want)
	}
}

func TestParse(t *testing.T) {
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
		{
			// A file may stop at its last line without ending it, and the
			// last line may be a marker: the exchange it opens is empty.
			name:    "a marker ends the file unterminated",
			in:      "-- a --",
			wantErr: "exchange a has no request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(f.Comment) != tt.head {
				t.Errorf("comment = %q, want %q", f.Comment, tt.head)
			}
			if len(f.Exchanges) != len(tt.want) {
				t.Fatalf("got %d exchanges, want %d", len(f.Exchanges), len(tt.want))
			}
			for i, s := range f.Exchanges {
				w := tt.want[i]
				if s.name != w.name || string(s.req) != w.req || string(s.resp) != w.resp {
					t.Errorf("exchange %d = %q, %q, %q\nwant %q, %q, %q",
						i, s.name, s.req, s.resp, w.name, w.req, w.resp)
				}
			}
		})
	}
}

// What parse reads, text writes: a file rr has already written comes back
// the same.
func TestParseRoundTrip(t *testing.T) {
	want := "notes\n\n-- a --\nGET https://x/ HTTP/1.1\n\nHTTP/1.1 200 OK\n\nhi\n-- b --\nGET https://y/ HTTP/1.1\n\n"
	f, err := Parse([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(text(&f)); got != want {
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
		{"--  --", "", true}, // a marker, but one parse refuses
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
