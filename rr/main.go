package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lotusirous/cmd/rr/http2curl"
)

const usageText = `usage: rr run [-match regexp] [-omit regexp] [-timeout duration] file...	send the requests in each file, store the responses in it
       rr fmt [-l] file...	format the requests in each file, send nothing
       rr gen [-match regexp] [-form form] file...	write the requests as curl commands
a file operand is a file, or a directory standing for the .rr files below it.
file holds exchanges, each opened by a line naming it:
	-- items/create --
	POST https://api.example.com/items HTTP/1.1
	Content-Type: application/json
	Authorization: Bearer ${TOKEN}

	{"name": "x"}

`

func usage() {
	fmt.Fprint(os.Stderr, usageText)
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("rr: ")

	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet("rr "+cmd, flag.ExitOnError)
	fs.Usage = usage
	var matchRe, omitRe, genForm *string
	var timeout *time.Duration
	var listOnly *bool
	switch cmd {
	case "run":
		matchRe = fs.String("match", "", "send only the exchanges whose name this matches")
		omitRe = fs.String("omit", "", "store no response header whose name this matches")
		timeout = fs.Duration("timeout", 0, "wait no longer than this for an exchange to answer")
	case "fmt":
		listOnly = fs.Bool("l", false, "name the files rr fmt would change, and change none")
	case "gen":
		matchRe = fs.String("match", "", "write only the exchanges whose name this matches")
		genForm = fs.String("form", "curl", "the form to write the request in")
	default:
		usage()
	}
	fs.Parse(args)
	if fs.NArg() == 0 {
		usage()
	}

	match, err := pattern(matchRe)
	if err != nil {
		log.Fatalf("-match: %v", err)
	}
	omit, err := pattern(omitRe)
	if err != nil {
		log.Fatalf("-omit: %v", err)
	}
	paths, err := files(fs.Args())
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "run":
		err = run(ctx, os.Stdout, paths, option{
			match: match,
			omit:  omit,
			client: &http.Client{
				Timeout:       *timeout,
				CheckRedirect: noRedirect,
			},
		})
	case "fmt":
		err = format(os.Stdout, paths, *listOnly)
	case "gen":
		err = gen(ctx, os.Stdout, paths, genOption{match: match, form: *genForm})
	}
	if err != nil {
		log.Fatal(err)
	}
}

func pattern(s *string) (*regexp.Regexp, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	return regexp.Compile(*s)
}

func files(args []string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)
	add := func(path string) {
		if path = filepath.Clean(path); !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			add(arg)
			continue
		}
		n, err := walk(arg, add)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, &fs.PathError{Op: "walk", Path: arg, Err: errors.New("no .rr file")}
		}
	}
	return out, nil
}

func walk(dir string, add func(string)) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			c, err := walk(path, add)
			if err != nil {
				return n, err
			}
			n += c
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".rr"):
			add(path)
			n++
		}
	}
	return n, nil
}

// parseFile reads and parses path, naming it in the error.
func parseFile(path string) (File, fs.FileMode, error) {
	data, mode, err := readFile(path)
	if err != nil {
		return File{}, 0, err
	}
	f, err := Parse(data)
	if err != nil {
		return File{}, 0, &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	return f, mode, nil
}

// noRedirect keeps the client from following redirects, so the stored
// response is the one the file's request produced.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type option struct {
	match, omit *regexp.Regexp
	client      *http.Client
}

// run sends the exchanges in each of paths in turn.
func run(ctx context.Context, w io.Writer, paths []string, opt option) error {
	sent := 0
	for _, path := range paths {
		n := 0
		for resp, err := range runFile(ctx, path, opt) {
			if err != nil {
				return err
			}
			if n == 0 && len(paths) > 1 {
				if _, err := io.WriteString(w, path+"\n"); err != nil {
					return err
				}
			}
			n++
			if _, err := w.Write(resp); err != nil {
				return err
			}
		}
		sent += n
	}
	if sent == 0 {
		return noMatch(paths)
	}
	return nil
}

// noMatch is the error for a pattern that named no exchange in any of paths.
// One file names itself, as every other error rr returns does; many files have
// no one name to give.
func noMatch(paths []string) error {
	err := errors.New("no exchange matches")
	if len(paths) == 1 {
		return &fs.PathError{Op: "match", Path: paths[0], Err: err}
	}
	return err
}

// runFile sends the exchanges in path that opt.match names and yields each
// response as it stores it. The file is written back whether the range runs
// out or the caller stops it, so what answered is stored either way.
func runFile(ctx context.Context, path string, opt option) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		f, mode, err := parseFile(path)
		if err != nil {
			yield(nil, err)
			return
		}

		sent, ok := 0, true
		for i, exchange := range f.Exchanges {
			if !isMatch(exchange, opt.match) {
				continue
			}

			var resp []byte
			if resp, err = send(ctx, exchange, path, opt); err != nil {
				break
			}
			f.Exchanges[i].Response = resp
			sent++

			var buf bytes.Buffer
			buf.WriteString(markerLine(exchange.Name))
			endLine(&buf, resp)
			if ok = yield(buf.Bytes(), nil); !ok {
				break
			}
		}
		if sent > 0 {
			err = errors.Join(err, os.WriteFile(path, text(&f), mode))
		}
		if err != nil && ok {
			yield(nil, err)
		}
	}
}

func newRequest(ctx context.Context, ex Exchange, path string) (*http.Request, error) {
	expanded, err := expandEnv(ex.Request)
	if err != nil {
		return nil, fail("expand", path, ex.Name, err)
	}
	req, err := parseRequest(ctx, expanded)
	if err != nil {
		return nil, fail("parse", path, ex.Name, err)
	}
	return req, nil
}

func send(ctx context.Context, ex Exchange, path string, opt option) ([]byte, error) {
	req, err := newRequest(ctx, ex, path)
	if err != nil {
		return nil, err
	}
	answer, err := opt.client.Do(req)
	if err != nil {
		return nil, fail("send", path, ex.Name, err)
	}
	resp, err := responseWire(answer, opt.omit)
	if err != nil {
		return nil, fail("read", path, ex.Name, err)
	}
	return resp, nil
}

func fail(op, path, name string, err error) error {
	return &fs.PathError{Op: op, Path: path, Err: fmt.Errorf("%s: %w", name, err)}
}

// format formats each of paths and writes to w the name of every file it changed.
func format(w io.Writer, paths []string, reportOnly bool) error {
	bad := 0
	for _, path := range paths {
		out, mode, err := formatFile(path)
		if err != nil {
			log.Print(err) // said as it is met: a walk of a tree is long
			bad++
			continue
		}
		if out == nil {
			continue
		}
		if !reportOnly {
			if err := os.WriteFile(path, out, mode); err != nil {
				log.Print(err)
				bad++
				continue
			}
		}
		if _, err := fmt.Fprintln(w, path); err != nil {
			return err
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d files not formatted", bad, len(paths))
	}
	return nil
}

// formatFile returns the requests in path in canonical form.
func formatFile(path string) ([]byte, fs.FileMode, error) {
	data, mode, err := readFile(path)
	if err != nil {
		return nil, 0, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, 0, &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	out := Format(&f)
	if bytes.Equal(out, data) {
		return nil, 0, nil
	}
	return out, mode, nil
}

type genOption struct {
	match *regexp.Regexp
	form  string
}

// gen writes the requests in each of paths in the form opt.form names.
func gen(ctx context.Context, w io.Writer, paths []string, opt genOption) error {
	written := 0
	for _, path := range paths {
		n := 0
		for cmd, err := range genFile(ctx, path, opt) {
			if err != nil {
				return err
			}
			if written > 0 { // a blank line between one command and the next
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			if n == 0 && len(paths) > 1 {
				if _, err := fmt.Fprintf(w, "# %s\n", path); err != nil {
					return err
				}
			}
			n, written = n+1, written+1
			if _, err := w.Write(cmd); err != nil {
				return err
			}
		}
	}
	if written == 0 {
		return noMatch(paths)
	}
	return nil
}

// genFile writes the requests in path in another form, yielding each command
// as one block.
func genFile(ctx context.Context, path string, opt genOption) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		var write func(io.Writer, *http.Request) error
		switch opt.form {
		case "curl":
			write = http2curl.Write
		default:
			yield(nil, fmt.Errorf("unknown form: %s", opt.form))
			return
		}

		f, _, err := parseFile(path)
		if err != nil {
			yield(nil, err)
			return
		}

		for _, ex := range f.Exchanges {
			if !isMatch(ex, opt.match) {
				continue
			}
			req, err := newRequest(ctx, ex, path)
			if err != nil {
				yield(nil, err)
				return
			}

			var buf bytes.Buffer
			fmt.Fprintf(&buf, "# %s\n", ex.Name)
			if err := write(&buf, req); err != nil {
				yield(nil, err)
				return
			}
			if !yield(buf.Bytes(), nil) {
				return
			}
		}
	}
}

func isMatch(e Exchange, re *regexp.Regexp) bool {
	return re == nil || re.MatchString(e.Name)
}

var reEnv = regexp.MustCompile(`\$(?:\{([A-Za-z_]\w*)\}|([A-Za-z_]\w*))`)

// expandEnv substitutes ${NAME} and $NAME from the environment. It makes a
// single pass, and it reports every unset name rather than only the last one.
func expandEnv(b []byte) ([]byte, error) {
	var (
		out     []byte
		last    int
		missing []string
	)
	for _, m := range reEnv.FindAllSubmatchIndex(b, -1) {
		lo, hi := m[2], m[3] // ${NAME}
		if lo < 0 {
			lo, hi = m[4], m[5] // $NAME
		}
		name := string(b[lo:hi])
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		out = append(out, b[last:m[0]]...)
		out = append(out, v...)
		last = m[1]
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset: %s", strings.Join(missing, ", "))
	}
	return append(out, b[last:]...), nil
}

// parseRequest parses wire-format HTTP with an absolute-form request URI. It
// takes b as the file writes it, which rr fmt need not have been over: either
// line ending, and a header block that may stop at the last header.
func parseRequest(ctx context.Context, b []byte) (*http.Request, error) {
	head, body := cutHead(b)
	// A request written by hand often stops after its last header. End the
	// block here, on the way to the wire, and not in the file: rewriting a
	// file is rr fmt's work.
	head = bytes.TrimRight(head, "\r\n")
	head = append(head[:len(head):len(head)], '\n', '\n')
	body = bytes.TrimSuffix(body, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	if len(bytes.TrimSpace(body)) == 0 {
		body = nil
	}

	r := bufio.NewReader(bytes.NewReader(head))
	in, err := http.ReadRequest(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no request")
		}
		return nil, err
	}
	if in.URL.Scheme != "http" && in.URL.Scheme != "https" {
		return nil, errors.New("request URI must be absolute (http:// or https://)")
	}

	req, err := http.NewRequestWithContext(ctx, in.Method, in.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = in.Header.Clone()
	req.Header.Del("Content-Length") // the body in the file sets these
	req.Header.Del("Transfer-Encoding")
	if in.Host != "" {
		req.Host = in.Host
	}
	return req, nil
}

// cutHead divides b at the blank line that ends the header block, the body
// being what follows it. A file writes its lines with LF or with CRLF and a
// request is framed the same either way, the line ending being the file's
// business and not the wire's. A block that stops at the last header has no
// such line, and no body.
func cutHead(b []byte) (head, body []byte) {
	for i := 0; i+1 < len(b); i++ {
		switch {
		case b[i] != '\n':
		case b[i+1] == '\n':
			return b[:i+2], b[i+2:]
		case b[i+1] == '\r' && i+2 < len(b) && b[i+2] == '\n':
			return b[:i+3], b[i+3:]
		}
	}
	return b, nil
}

// responseWire reads and closes resp's body and returns the response in
// wire format, indenting JSON bodies and leaving out the headers omit names.
func responseWire(resp *http.Response, omit *regexp.Regexp) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	// Shape the body by what the server said it was, before any of the saying
	// is dropped: -omit says what the file keeps, not what a body is.
	if isJSON(resp.Header.Get("Content-Type")) {
		body = indentJSON(body)
	}
	omitHeaders(resp, omit)

	// Store the body as received rather than as framed: indenting changes its
	// length, and a chunked body has no length to keep.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))

	var buf bytes.Buffer
	if err := resp.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// omitHeaders drops from resp the fields whose canonical name re matches: the
// chatty half of an answer, a Date that moves every run and a request id that
// is new every time, which a file is a better diff without. A nil re drops
// nothing.
//
// Connection is cleared rather than deleted, and Content-Length outlives any
// re: [http.Response.Write] writes both from the response, and the length,
// counted after the body is indented, is rr's own framing of what it stores.
func omitHeaders(resp *http.Response, re *regexp.Regexp) {
	if re == nil {
		return
	}
	for name := range resp.Header {
		if re.MatchString(name) {
			delete(resp.Header, name) // the keys are canonical: Del would only recanonicalize
		}
	}
	if re.MatchString("Connection") {
		resp.Close = false
	}
}

func isJSON(contentType string) bool {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediatype == "application/json" || strings.HasSuffix(mediatype, "+json")
}

func indentJSON(body []byte) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(body), "", "  "); err != nil {
		return body
	}
	return out.Bytes()
}
