package main

// The cases live in testdata, one to a file, named for the command it runs:
// TestRun drives run_*.txt, TestFmt fmt_*.txt, and so on.
//
// A line beginning ">>> " opens a section; every other line is its text, as
// the case writes it:
//
//	>>> f.rr		a file the case plants, "dir/" for a directory
//	>>> want f.rr		what f.rr holds once the command has run
//	>>> stdout		what the command writes
//
// A file with no want must come out untouched: the same bytes and the same
// modification time, so a command that rewrites what it need not is caught.
//
// Above the first section, "#" opens a directive, the first of them naming the
// command the file is named for:
//
//	run [-match re] [-omit re]	the command, and its flags
//	fmt [-l]
//	gen [-match re] [-form name]
//	walk				the operands, as files reduces them
//	args f...			the operands, in place of every file
//	error text			the command must fail, saying text
//	env NAME=value			set for the length of the case
//	expect Name: value		the server checks the request carries it
//	reply Name: value		the server answers with it
//	mode 640			the files are planted with it, and keep it
//	echo				the server answers with the request body
//	chunked				the server answers without a length
//	crlf				the files are planted with CRLF endings
//	twice				the command runs twice
//	note text			prose
//
// {{root}} is the directory the files are planted in, {{url}} the test server.

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	urlMark  = "{{url}}"
	rootMark = "{{root}}"
)

// planted is the modification time files are planted with: one still carrying
// it was not rewritten.
var planted = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func TestRun(t *testing.T)  { testCases(t, "run") }
func TestFmt(t *testing.T)  { testCases(t, "fmt") }
func TestGen(t *testing.T)  { testCases(t, "gen") }
func TestWalk(t *testing.T) { testCases(t, "walk") }

func testCases(t *testing.T, cmd string) {
	files, err := filepath.Glob("testdata/" + cmd + "_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no %s cases in testdata", cmd)
	}
	for _, file := range files {
		c := readCase(t, file)
		if c.cmd != cmd {
			t.Fatalf("%s: named for %s, but it is a %s case", file, cmd, c.cmd)
		}
		name := strings.TrimSuffix(filepath.Base(file), ".txt")
		t.Run(strings.TrimPrefix(name, cmd+"_"), func(t *testing.T) { runCase(t, c) })
	}
}

type testCase struct {
	file string

	cmd                 string
	args                []string
	match, omit, form   string
	list                bool
	wantErr             string
	env, expect, reply  []string
	mode                fs.FileMode
	echo, chunked, crlf bool
	twice               bool

	order  []string // the files, in the order the case gives them
	files  map[string]*string
	wants  map[string]*string
	stdout *string // nil if the case says nothing about it

	root string
	url  string // empty unless the command is run
}

func readCase(t *testing.T, file string) *testCase {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	c := &testCase{
		file:  file,
		files: make(map[string]*string),
		wants: make(map[string]*string),
	}
	var sec *string
	for line := range strings.Lines(string(data)) {
		switch name, ok := strings.CutPrefix(line, ">>> "); {
		case ok:
			sec = section(c, strings.TrimSpace(name))
		case sec != nil:
			*sec += line
		case strings.HasPrefix(line, "#"):
			directive(t, c, strings.TrimSpace(line[1:]))
		case strings.TrimSpace(line) != "":
			t.Fatalf("%s: %q is outside a section", file, strings.TrimRight(line, "\n"))
		}
	}
	return c
}

// section returns where the text under a ">>> " line is kept.
func section(c *testCase, name string) *string {
	if name == "stdout" {
		if c.stdout == nil {
			c.stdout = new(string)
		}
		return c.stdout
	}
	m := c.files
	if path, ok := strings.CutPrefix(name, "want "); ok {
		m, name = c.wants, strings.TrimSpace(path)
	} else {
		c.order = append(c.order, name)
	}
	s := new(string)
	m[name] = s
	return s
}

func directive(t *testing.T, c *testCase, s string) {
	t.Helper()
	verb, rest, _ := strings.Cut(s, " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "run", "fmt", "gen", "walk":
		c.cmd = verb
		set := flag.NewFlagSet(verb, flag.ContinueOnError)
		set.SetOutput(io.Discard)
		set.StringVar(&c.match, "match", "", "")
		set.StringVar(&c.omit, "omit", "", "")
		set.StringVar(&c.form, "form", "curl", "")
		set.BoolVar(&c.list, "l", false, "")
		if err := set.Parse(strings.Fields(rest)); err != nil {
			t.Fatalf("%s: %s: %v", c.file, s, err)
		}
	case "args":
		c.args = strings.Fields(rest)
	case "error":
		c.wantErr = rest
	case "env":
		c.env = append(c.env, rest)
	case "expect":
		c.expect = append(c.expect, rest)
	case "reply":
		c.reply = append(c.reply, rest)
	case "mode":
		n, err := strconv.ParseUint(rest, 8, 32)
		if err != nil {
			t.Fatalf("%s: mode %s: %v", c.file, rest, err)
		}
		c.mode = fs.FileMode(n)
	case "echo":
		c.echo = true
	case "chunked":
		c.chunked = true
	case "crlf":
		c.crlf = true
	case "twice":
		c.twice = true
	case "note", "":
	default:
		t.Fatalf("%s: unknown directive %q", c.file, verb)
	}
}

func runCase(t *testing.T, c *testCase) {
	log.SetOutput(io.Discard) // rr fmt logs what it could not format
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	for _, e := range c.env {
		name, value, _ := strings.Cut(e, "=")
		t.Setenv(name, value)
	}
	var srv *httptest.Server
	if c.cmd == "run" {
		srv = serveCase(t, c)
		c.url = srv.URL
	}
	plant(t, c)

	var out bytes.Buffer
	err := execCase(t, c, srv, &out)
	if err == nil && c.twice {
		out.Reset() // what the second run says is what changed
		err = execCase(t, c, srv, &out)
	}
	switch {
	case c.wantErr == "":
		if err != nil {
			t.Fatal(err)
		}
	case err == nil:
		t.Fatalf("err = nil, want it to say %q", c.wantErr)
	case !strings.Contains(canon(c, err.Error()), c.wantErr):
		t.Fatalf("err = %v, want it to say %q", err, c.wantErr)
	}

	if c.stdout != nil {
		if got := canon(c, out.String()); got != *c.stdout {
			t.Errorf("stdout:\n%s\nwant:\n%s", got, *c.stdout)
		}
	}
	checkFiles(t, c)
}

func execCase(t *testing.T, c *testCase, srv *httptest.Server, out *bytes.Buffer) error {
	t.Helper()
	switch c.cmd {
	case "run":
		return run(t.Context(), out, paths(c), option{
			match:  mustPattern(t, c.file, c.match),
			omit:   mustPattern(t, c.file, c.omit),
			client: testClient(srv),
		})
	case "fmt":
		return format(out, paths(c), c.list)
	case "gen":
		return gen(t.Context(), out, paths(c), genOption{
			form:  c.form,
			match: mustPattern(t, c.file, c.match),
		})
	case "walk":
		found, err := files(paths(c))
		for _, path := range found {
			fmt.Fprintln(out, path)
		}
		return err
	}
	t.Fatalf("%s: unknown command %q", c.file, c.cmd)
	return nil
}

func checkFiles(t *testing.T, c *testCase) {
	t.Helper()
	for path := range c.wants {
		if _, ok := c.files[path]; !ok {
			t.Fatalf("%s: a want for %s, which the case never plants", c.file, path)
		}
	}
	for _, path := range c.order {
		if strings.HasSuffix(path, "/") {
			continue
		}
		full := filepath.Join(c.root, path)
		info, serr := os.Stat(full)
		data, rerr := os.ReadFile(full)
		if err := errors.Join(serr, rerr); err != nil {
			t.Fatal(err)
		}
		want, rewritten := c.wants[path]
		if !rewritten {
			want = c.files[path]
			if !info.ModTime().Equal(planted) {
				t.Errorf("%s was rewritten, want it left as it was planted", path)
			}
		}
		if c.mode != 0 && info.Mode().Perm() != c.mode {
			t.Errorf("%s mode = %v, want %v", path, info.Mode().Perm(), c.mode)
		}
		if got := canon(c, string(data)); got != *want {
			t.Errorf("%s:\n%s\nwant:\n%s", path, got, *want)
		}
	}
}

func plant(t *testing.T, c *testCase) {
	t.Helper()
	c.root = t.TempDir()
	mode := fs.FileMode(0o644)
	if c.mode != 0 {
		mode = c.mode
	}
	for _, path := range c.order {
		full := filepath.Join(c.root, path)
		dir := filepath.Dir(full)
		if strings.HasSuffix(path, "/") {
			dir = full
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if dir == full {
			continue
		}
		body := expand(c, *c.files[path])
		if c.crlf {
			body = strings.ReplaceAll(body, "\n", "\r\n")
		}
		err := errors.Join(
			os.WriteFile(full, []byte(body), mode),
			os.Chtimes(full, planted, planted),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// operands returns what # args names, or every file the case plants.
func operands(c *testCase) []string {
	if len(c.args) > 0 {
		return c.args
	}
	return c.order
}

func paths(c *testCase) []string {
	out := make([]string, 0, len(operands(c)))
	for _, arg := range operands(c) {
		out = append(out, filepath.Join(c.root, arg))
	}
	return out
}

// expand puts the real directory and server in place of the marks; canon puts
// them back, and the line ending with them.
func expand(c *testCase, s string) string {
	s = strings.ReplaceAll(s, rootMark, c.root)
	if c.url != "" {
		s = strings.ReplaceAll(s, urlMark, c.url)
	}
	return s
}

func canon(c *testCase, s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, c.root, rootMark)
	if c.url != "" {
		s = strings.ReplaceAll(s, c.url, urlMark)
	}
	return s
}

func mustPattern(t *testing.T, file, s string) *regexp.Regexp {
	t.Helper()
	re, err := pattern(&s)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return re
}

type reply struct {
	method string
	uri    string
	resp   *http.Response
}

// replies is the server's script, and a run case needs no other: the responses
// the want files store, in the order the run asks for them.
func replies(t *testing.T, c *testCase) []reply {
	t.Helper()
	match := mustPattern(t, c.file, c.match)
	var out []reply
	for _, path := range operands(c) {
		want, ok := c.wants[path]
		if !ok {
			continue
		}
		f, err := Parse([]byte(*want))
		if err != nil {
			t.Fatalf("%s: the want for %s does not parse: %v", c.file, path, err)
		}
		for _, ex := range f.Exchanges {
			if len(ex.Response) == 0 || !isMatch(ex, match) {
				continue
			}
			resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(ex.Response)), nil)
			if err != nil {
				t.Fatalf("%s: %s stores no response a server could send: %v", c.file, ex.Name, err)
			}
			line, _, _ := strings.Cut(string(ex.Request), "\n")
			method, rest, _ := strings.Cut(line, " ")
			uri, _, _ := strings.Cut(rest, " ")
			out = append(out, reply{strings.ToUpper(method), strings.TrimPrefix(uri, urlMark), resp})
		}
	}
	return out
}

func serveCase(t *testing.T, c *testCase) *httptest.Server {
	t.Helper()
	answers := replies(t, c)
	n := 0
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if err := expected(c, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
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
		if n == len(answers) {
			http.Error(w, "the case stores no reply for this request", http.StatusInternalServerError)
			return
		}
		answer := answers[n]
		n++
		if r.Method != answer.method || r.URL.RequestURI() != answer.uri {
			http.Error(w, r.Method+" "+r.URL.RequestURI()+", want "+answer.method+" "+answer.uri, http.StatusBadRequest)
			return
		}
		maps.Copy(w.Header(), answer.resp.Header)
		for _, h := range c.reply {
			name, value, _ := strings.Cut(h, ":")
			w.Header().Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
		if c.chunked {
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(answer.resp.StatusCode)
		io.Copy(w, answer.resp.Body)
	})
}

func expected(c *testCase, r *http.Request) error {
	want := make(http.Header)
	for _, h := range c.expect {
		name, value, _ := strings.Cut(h, ":")
		want.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	for name, values := range want {
		got := r.Header[name]
		if name == "Host" {
			got = []string{r.Host}
		}
		if !slices.Equal(got, values) {
			return fmt.Errorf("%s is %q, want %q", name, got, values)
		}
	}
	return nil
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil // a file is a better diff without one
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.CheckRedirect = noRedirect
	return c
}
