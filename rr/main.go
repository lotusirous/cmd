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
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lotusirous/cmd/rr/http2curl"
)

const usageText = `usage: rr run [-match regexp] [-omit regexp] [-timeout duration] file	send the requests in file, store the responses in it
       rr fmt file	format the requests in file, send nothing
       rr gen [-match regexp] [-form form] file	write the requests as curl commands
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
	var matchFlag, omitFlag, formFlag *string
	var timeoutFlag *time.Duration
	switch cmd {
	case "run":
		matchFlag = fs.String("match", "", "send only the exchanges whose name this matches")
		omitFlag = fs.String("omit", "", "store no response header whose name this matches")
		timeoutFlag = fs.Duration("timeout", 0, "wait no longer than this for an exchange to answer")
	case "fmt":
	case "gen":
		matchFlag = fs.String("match", "", "write only the exchanges whose name this matches")
		formFlag = fs.String("form", "curl", "the form to write the request in")
	default:
		usage()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	path := fs.Arg(0)

	match, err := pattern(matchFlag)
	if err != nil {
		log.Fatalf("-match: %v", err)
	}
	omit, err := pattern(omitFlag)
	if err != nil {
		log.Fatalf("-omit: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "run":
		err = run(ctx, path, option{
			match: match,
			omit:  omit,
			client: &http.Client{
				Timeout:       *timeoutFlag, // zero waits as long as the server takes
				CheckRedirect: noRedirect,
			},
			out: os.Stdout,
		})
	case "fmt":
		err = formatFile(path)
	case "gen":
		err = gen(ctx, *formFlag, path, match, os.Stdout)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// pattern compiles a flag that holds one: -match or -omit. It is nil for a
// command that has no such flag and empty for one whose flag went unwritten,
// and either leaves the caller its own default, every exchange or no header.
func pattern(s *string) (*regexp.Regexp, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	return regexp.Compile(*s)
}

// noRedirect keeps the client from following redirects, so the stored
// response is the one the file's request produced.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// An option says how a run is to be made: which exchanges to send, which of
// the headers they answer with to store, and what to send and report them
// with. It is what the flags of rr run come to, and the zero value of each
// field is the flag left unwritten. Nothing writes to one, so it is passed
// by value and read where it is wanted.
type option struct {
	// match selects the exchanges to send by name, as -match does. A nil
	// match, which is what an unwritten flag leaves, sends every one.
	match *regexp.Regexp

	// omit names the response headers not to store, as -omit does: it is
	// matched against the canonical name of each header of an answer, and
	// what it matches is left out of the file. A nil omit, which is what an
	// unwritten flag leaves, stores every header. Content-Length is rr's own
	// framing of the body it stores and outlives any omit; see [omitHeaders].
	omit *regexp.Regexp

	client *http.Client // what to send with
	out    io.Writer    // where to report each answer as it is stored
}

// run sends the exchanges in path that opt.match names and stores each
// response under the request that made it, less the headers opt.omit names.
// It stops at the first failure, and stores what the exchanges above it
// answered: they did happen, and the file is where rr says so.
//
// Every error it returns names the file, an *fs.PathError from the os call
// that failed or made here, so all of them read alike. A run can fail twice
// over, and then it returns the two joined.
func run(ctx context.Context, path string, opt option) error {
	data, mode, err := readFile(path)
	if err != nil {
		return err
	}
	f, err := Parse(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	sent, err := sendAll(ctx, f.Exchanges, path, opt)
	if sent == 0 {
		return err // nothing answered: the file is left as it was
	}

	return errors.Join(err, os.WriteFile(path, text(&f), mode))
}

// sendAll sends the exchanges opt.match names, in the order the file has
// them, storing each response under the request that made it and reporting it
// to opt.out as it goes. It stops at the first failure, an exchange being
// free to rely on the one above it.
//
// A request goes as the file writes it. Putting one in canonical form is rr
// fmt's work and no part of a run.
//
// It returns how many exchanges it stored: the caller writes the file only
// when that is not zero.
func sendAll(ctx context.Context, exchanges []Exchange, path string, opt option) (int, error) {
	sel := pick(exchanges, opt.match)
	if len(sel) == 0 {
		return 0, &fs.PathError{Op: "match", Path: path, Err: errors.New("no exchange matches")}
	}
	for n, i := range sel {
		resp, err := send(ctx, exchanges[i], path, opt)
		if err != nil {
			return n, err
		}
		exchanges[i].resp = resp
		if err := report(opt.out, exchanges[i]); err != nil {
			return n + 1, err // the answer is stored: only the saying of it failed
		}
	}
	return len(sel), nil
}

// report writes ex to w as the file keeps it, so that what rr prints and
// what it stores are the same text under the same name.
func report(w io.Writer, ex Exchange) error {
	var buf bytes.Buffer
	buf.WriteString(markerLine(ex.name))
	endLine(&buf, ex.resp)
	_, err := w.Write(buf.Bytes())
	return err
}

// send expands and sends ex's request as the file writes it, and returns the
// response in wire format, less the headers opt.omit names.
func send(ctx context.Context, ex Exchange, path string, opt option) ([]byte, error) {
	expanded, err := expandEnv(ex.req)
	if err != nil {
		return nil, fail("expand", path, ex.name, err)
	}
	out, err := parseRequest(ctx, expanded)
	if err != nil {
		return nil, fail("parse", path, ex.name, err)
	}
	answer, err := opt.client.Do(out)
	if err != nil {
		return nil, fail("send", path, ex.name, err)
	}
	resp, err := responseWire(answer, opt.omit)
	if err != nil {
		return nil, fail("read", path, ex.name, err)
	}
	return resp, nil
}

// fail returns the error to report for a failure in the exchange named name.
// The path stays the file's, so it is still one something can open, and the
// exchange is named in the error under it.
func fail(op, path, name string, err error) error {
	return &fs.PathError{Op: op, Path: path, Err: fmt.Errorf("%s: %w", name, err)}
}

// formatFile rewrites the request half of every exchange in path in canonical
// form and sends nothing. The comment and the stored responses are left
// exactly as they were; the marker lines are written the one way they are
// written, a name being the same name however it was spaced.
func formatFile(path string) error {
	data, mode, err := readFile(path)
	if err != nil {
		return err
	}
	f, err := Parse(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	return os.WriteFile(path, Format(&f), mode)
}

// gen writes the requests in path that re matches in another form, sending
// nothing. It expands the way run does, so what it writes is what run would
// send, values and all. Each is named in a comment above it, which
// survives a pipe to a shell and says which request it is.
func gen(ctx context.Context, form, path string, re *regexp.Regexp, w io.Writer) error {
	var write func(io.Writer, *http.Request) error
	switch form {
	case "curl":
		write = http2curl.Write
	default:
		return fmt.Errorf("unknown form: %s", form)
	}

	data, _, err := readFile(path)
	if err != nil {
		return err
	}
	f, err := Parse(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	sel := pick(f.Exchanges, re)
	if len(sel) == 0 {
		return &fs.PathError{Op: "match", Path: path, Err: errors.New("no exchange matches")}
	}

	for n, i := range sel {
		ex := f.Exchanges[i]
		expanded, err := expandEnv(ex.req)
		if err != nil {
			return fail("expand", path, ex.name, err)
		}
		req, err := parseRequest(ctx, expanded)
		if err != nil {
			return fail("parse", path, ex.name, err)
		}
		if n > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "# %s\n", ex.name); err != nil {
			return err
		}
		if err := write(w, req); err != nil {
			return err
		}
	}
	return nil
}

// pick returns the indexes of the exchanges whose name re matches, in the
// order the file has them. A nil re, which is what no -match flag leaves,
// matches every one.
func pick(exchanges []Exchange, re *regexp.Regexp) []int {
	var sel []int
	for i, s := range exchanges {
		if re == nil || re.MatchString(s.name) {
			sel = append(sel, i)
		}
	}
	return sel
}

var reEnv = regexp.MustCompile(`\$(?:\{([A-Za-z_]\w*)\}|([A-Za-z_]\w*))`)

// expandEnv substitutes ${NAME} and $NAME from the environment. It makes a
// single pass, so a substituted value is never rescanned, and it reports
// every unset name rather than only the last one.
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
	// Parse the header block alone and take the body from the file:
	// http.ReadRequest reads only as far as the framing headers say.
	head, body := cutHead(b)
	// A request written by hand often stops after its last header, leaving
	// the block unterminated but unambiguous. It is ended here, on the way to
	// the wire, and not in the file: rr fmt is what rewrites a file.
	head = bytes.TrimRight(head, "\r\n")
	head = append(head[:len(head):len(head)], '\n', '\n')
	body = bytes.TrimSuffix(body, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	if len(bytes.TrimSpace(body)) == 0 {
		body = nil // a trailing blank line is not a body
	}

	in, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no request")
		}
		return nil, err
	}
	if in.URL.Scheme != "http" && in.URL.Scheme != "https" {
		return nil, errors.New("request URI must be absolute (http:// or https://)")
	}
	// ReadRequest yields a server-side request, which a client cannot send;
	// rebuild it from the URL and copy over what the file asked for.
	out, err := http.NewRequestWithContext(ctx, in.Method, in.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header = in.Header.Clone()
	out.Header.Del("Content-Length") // the body in the file sets these
	out.Header.Del("Transfer-Encoding")
	if in.Host != "" {
		out.Host = in.Host
	}
	return out, nil
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
	// The body is shaped by what the server said it was, before any of the
	// saying is dropped: -omit says what the file keeps, not what a body is.
	if isJSON(resp.Header.Get("Content-Type")) {
		body = indentJSON(body)
	}
	omitHeaders(resp, omit)

	// Store the body as received rather than as framed: indenting changes
	// its length, and a chunked body has no length to keep.
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

// omitHeaders drops from resp the fields whose canonical name re matches:
// the chatty half of an answer, a Date that moves every run and a request id
// that is new every time, which a file is a better diff without. A nil re,
// which is what an unwritten -omit leaves, drops nothing.
//
// Connection is cleared rather than deleted, [http.Response.Write] writing it
// from the response and not from its headers. Content-Length is written from
// there as well and outlives any re: counted after the body is indented, it
// is rr's own framing of the text it stores rather than a header the server
// sent.
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

// isJSON reports whether a Content-Type declares a JSON body: application/json
// or a +json suffix, with any parameters. A body that merely parses as JSON is
// not ours to reformat.
func isJSON(contentType string) bool {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediatype == "application/json" || strings.HasSuffix(mediatype, "+json")
}

// indentJSON pretty-prints a JSON body with a two-space indent. A body that
// is not JSON is returned unchanged.
func indentJSON(body []byte) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(body), "", "  "); err != nil {
		return body
	}
	return out.Bytes()
}
