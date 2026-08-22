/*
Rr sends the HTTP requests stored in a file and writes each response back into
the same file, under the request that made it. The name is those two halves:
request and response.

The file is plain text and holds the protocol itself: each request as it goes
on the wire, each response as it came back. Its layout is txtar's, the
archive format the Go tools keep their test cases in, and so are its goals:

  - be trivial enough to write and edit by hand.
  - be HTTP and nothing else, but for the line that names an exchange.
  - diff nicely in git history and code reviews.

Non-goals include being a scripting language, asserting anything about a
response, carrying state from one exchange to the next, and storing a body
that is not text.

Usage:

	rr run [-match regexp] [-omit regexp] file
	rr fmt file
	rr gen [-match regexp] [-form form] file

Rr run sends the requests and stores the responses. Rr fmt only formats the
file, sending nothing and leaving any stored response alone. Rr gen writes the
requests in another form and sends nothing either; curl is the form it writes
when none is named, and so far the only one there is.

A file holds a collection of exchanges, each opened by a txtar marker line
naming it:

	-- items/create --
	POST https://api.example.com/items HTTP/1.1
	Content-Type: application/json

	{
	  "name": "x"
	}
	HTTP/1.1 201 Created
	Content-Type: application/json
	Content-Length: 28

	{
	  "id": 7,
	  "name": "x"
	}

Text before the first such line is a comment rr does not read. A name is the
writer's own, and it is what -match selects on and what an error quotes, so no
two exchanges in a file may answer to one. Rr sends them in the order the file
has them and stops at the first failure, an exchange being free to rely on the
one above it.

An exchange is a wire-format HTTP request with an absolute-form request URI,
followed by the response last stored for it. The response begins at the first
line that begins one: HTTP/, a version, and a status code. The request is put
in canonical form before it is sent, so a file written by hand ends up stored
the way rr would have written it.

A response is stored as it came back, less the headers -omit names. The
pattern is matched against the canonical name of each one and is unanchored,
as -match is: -omit '^(Date|X-Amz-)' keeps out a date that moves every run
and a request id that is new every time, so what a diff is left with is what
changed. It says what the file keeps and no more. The request is the writer's
own text and keeps every line of it, and Content-Length outlives any pattern,
being rr's framing of the body it stores rather than a header the server
sent.

${NAME} and $NAME are expanded from the environment before sending, but the
file on disk keeps the unexpanded text, so it stays safe to commit. Rr gen
expands them the same way, so what it writes carries the values and is not
itself safe to commit.

Everything after the blank line is sent as the body, less the newline that
ends it. Rr frames the request itself, so Content-Length need not be written
by hand, nor kept up to date when the body changes.

Formatting is what rr run does to a request before it goes out, and all that
rr fmt does. A known method is upper-cased, CRLF and folded lines are undone,
each header colon is followed by one space, the standard header names are
respelled and written ahead of the custom ones, whose casing is their
author's, and Content-Length and Transfer-Encoding are dropped, rr framing
the request. Header lines sharing a name keep their order. The text is
rewritten and not the request: ${NAME} is left unexpanded, and formatting a
formatted file changes nothing.

A body is indented only when Content-Type declares it JSON: application/json,
or a type ending in +json. A body that merely parses as JSON is left alone,
nothing having said it was JSON, and so is one that says it is JSON and does
not parse. Rr fmt formats every request in the file, there being no -match,
and touches nothing else: the comment and the stored responses stay as they
were.

Rr gen writes each matching request as a command for another program, on
standard output, and leaves the file alone. The form is -form, and curl is
the default and so far the only one. Each command is written under the name
of its exchange, as a comment, so a pipe to a shell says which request is
which. The request is formatted and expanded first, so what gen writes is
what run would send, values and all, and is not itself safe to commit.

The curl form says what the request says and no more: no -s, no -i, no -L.
Header names come out canonical and sorted, X-API-KEY as X-Api-Key, an
[http.Header] having kept neither the spelling nor the order the file had. A
body goes in --data-raw, which keeps the newlines an indented JSON body has;
one that is not text is read from standard input instead.
*/
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
	"github.com/lotusirous/cmd/rr/httpfmt"
)

// The two ends of a marker, the line that opens an exchange. A line that
// begins with one, ends with the other, and has room for a name between is a
// marker; every other line is text, so a body line of dashes stays a body
// line.
var (
	markPre = []byte("-- ")
	markSuf = []byte(" --")
)

const usageText = `usage: rr run [-match regexp] [-omit regexp] file	send the requests in file, store the responses in it
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
	switch cmd {
	case "run":
		matchFlag = fs.String("match", "", "send only the exchanges whose name this matches")
		omitFlag = fs.String("omit", "", "store no response header whose name this matches")
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
				Timeout:       30 * time.Second,
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

// A section is one named exchange: the request as the file has it, and the
// response last stored under it, empty until the request has been sent.
type section struct {
	name string
	req  []byte
	resp []byte
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
// Every error it returns is an *fs.PathError naming the file, either from the
// os call that failed or made here, so all of them read alike.
func run(ctx context.Context, path string, opt option) error {
	data, mode, err := readFile(path)
	if err != nil {
		return err
	}
	head, secs, err := splitFile(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	sel := pick(secs, opt.match)
	if len(sel) == 0 {
		return &fs.PathError{Op: "match", Path: path, Err: errors.New("no exchange matches")}
	}

	var sendErr error
	sent := 0
	for _, i := range sel {
		req, resp, err := send(ctx, secs[i], path, opt)
		if err != nil {
			sendErr = err
			break
		}
		secs[i].req, secs[i].resp = req, resp
		sent++

		// Report the exchange as the file keeps it, so what rr prints and
		// what it stores are the same text under the same name.
		var buf bytes.Buffer
		buf.WriteString(marker(secs[i].name))
		endLine(&buf, resp)
		if _, err := opt.out.Write(buf.Bytes()); err != nil {
			return err
		}
	}
	if sent == 0 {
		return sendErr // nothing answered: leave the file as it was
	}
	if err := rewriteFile(path, mode, head, secs); err != nil && sendErr == nil {
		sendErr = err
	}
	return sendErr
}

// send formats, expands and sends sec's request. It returns the request as it
// is to be stored and the response in wire format, less the headers opt.omit
// names; the request is stored formatted, so what the file keeps is what went
// out.
func send(ctx context.Context, sec section, path string, opt option) (req, resp []byte, err error) {
	req = httpfmt.Format(sec.req)
	expanded, err := expandEnv(req)
	if err != nil {
		return nil, nil, fail("expand", path, sec.name, err)
	}
	out, err := parseRequest(ctx, expanded)
	if err != nil {
		return nil, nil, fail("parse", path, sec.name, err)
	}
	answer, err := opt.client.Do(out)
	if err != nil {
		return nil, nil, fail("send", path, sec.name, err)
	}
	resp, err = responseWire(answer, opt.omit)
	if err != nil {
		return nil, nil, fail("read", path, sec.name, err)
	}
	return req, resp, nil
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
	head, secs, err := splitFile(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	for i := range secs {
		secs[i].req = httpfmt.Format(secs[i].req)
	}
	return rewriteFile(path, mode, head, secs)
}

// gen writes the requests in path that re matches in another form, sending
// nothing. It formats and expands the way run does, so what it writes is what
// run would send, values and all. Each is named in a comment above it, which
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
	_, secs, err := splitFile(data)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}
	sel := pick(secs, re)
	if len(sel) == 0 {
		return &fs.PathError{Op: "match", Path: path, Err: errors.New("no exchange matches")}
	}

	for n, i := range sel {
		expanded, err := expandEnv(httpfmt.Format(secs[i].req))
		if err != nil {
			return fail("expand", path, secs[i].name, err)
		}
		req, err := parseRequest(ctx, expanded)
		if err != nil {
			return fail("parse", path, secs[i].name, err)
		}
		if n > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "# %s\n", secs[i].name); err != nil {
			return err
		}
		if err := write(w, req); err != nil {
			return err
		}
	}
	return nil
}

// readFile returns the contents of path and the mode to store it back under.
// A directory needs no test of its own: os.ReadFile reports one.
func readFile(path string) ([]byte, os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), err
}

// splitFile returns the text before the first marker line, which is a comment,
// and the exchanges the markers open. A name has to be there and has to be its
// own: it is what -match selects and what an error quotes, so two exchanges
// answering to one name is a file written wrong.
func splitFile(data []byte) (head []byte, secs []section, err error) {
	seen := make(map[string]bool)
	head = data
	name, start := "", 0
	for off := 0; off < len(data); {
		line, _, _ := bytes.Cut(data[off:], []byte("\n"))
		next := off + len(line) + 1
		mark, ok := markerName(bytes.TrimSuffix(line, []byte("\r")))
		if !ok {
			off = next
			continue
		}
		if name == "" {
			head = data[:off] // nothing is open yet: this opens the first
		} else {
			secs = append(secs, cut(name, data[start:off]))
		}
		switch {
		case mark == "":
			return nil, nil, errors.New("exchange with no name")
		case seen[mark]:
			return nil, nil, fmt.Errorf("two exchanges named %s", mark)
		}
		seen[mark] = true
		name, start, off = mark, next, next
	}
	if name != "" {
		secs = append(secs, cut(name, data[start:]))
	}
	if len(secs) == 0 {
		return nil, nil, errors.New("no exchange")
	}
	for _, s := range secs {
		if len(bytes.TrimSpace(s.req)) == 0 {
			return nil, nil, fmt.Errorf("exchange %s has no request", s.name)
		}
	}
	return head, secs, nil
}

// cut returns the exchange that name and text hold, dividing text at the line
// that begins the stored response. One that has never been sent has no such
// line, and no response.
func cut(name string, text []byte) section {
	for off := 0; off < len(text); {
		line, _, _ := bytes.Cut(text[off:], []byte("\n"))
		if isStatus(bytes.TrimSuffix(line, []byte("\r"))) {
			return section{name, text[:off], text[off:]}
		}
		off += len(line) + 1
	}
	return section{name: name, req: text}
}

// markerName returns the name a marker line holds, and whether line is one.
func markerName(line []byte) (string, bool) {
	if len(line) < len(markPre)+len(markSuf) ||
		!bytes.HasPrefix(line, markPre) || !bytes.HasSuffix(line, markSuf) {
		return "", false
	}
	return string(bytes.TrimSpace(line[len(markPre) : len(line)-len(markSuf)])), true
}

// marker returns the line that opens the exchange named name.
func marker(name string) string {
	return string(markPre) + name + string(markSuf) + "\n"
}

// isStatus reports whether line begins a stored response, as
// [http.Response.Write] writes one: HTTP/1.1 200 OK. The version is read
// rather than assumed, a request over TLS having possibly negotiated HTTP/2.
func isStatus(line []byte) bool {
	version, rest, ok := bytes.Cut(line, []byte(" "))
	if !ok || !bytes.HasPrefix(version, []byte("HTTP/")) {
		return false
	}
	major, minor, ok := bytes.Cut(version[len("HTTP/"):], []byte("."))
	if !ok || !digits(major) || !digits(minor) {
		return false
	}
	code, _, _ := bytes.Cut(rest, []byte(" "))
	return len(code) == 3 && digits(code)
}

// digits reports whether b is one or more ASCII digits and nothing else.
func digits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// pick returns the indexes of the exchanges whose name re matches, in the
// order the file has them. A nil re, which is what no -match flag leaves,
// matches every one.
func pick(secs []section, re *regexp.Regexp) []int {
	var sel []int
	for i, s := range secs {
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

// parseRequest parses wire-format HTTP with an absolute-form request URI.
// It takes b as [httpfmt.Format] leaves it: LF line endings, and a header
// block ending at the first blank line.
func parseRequest(ctx context.Context, b []byte) (*http.Request, error) {
	// Parse the header block alone and take the body from the file:
	// http.ReadRequest reads only as far as the framing headers say.
	var body []byte
	head := b
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		head, body = b[:i+2], b[i+2:]
	}
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

// rewriteFile stores head and secs in path. Writing in place keeps the mode,
// the owner, and any link or symbolic link to the file; mode applies only if
// it has gone missing since it was read. A write that fails midway leaves the
// file truncated, which is the price of not renaming a temporary over it: the
// rename replaces the file, and with it everything the file system knew about
// the one the user made.
func rewriteFile(path string, mode os.FileMode, head []byte, secs []section) error {
	var buf bytes.Buffer
	endLine(&buf, head)
	for _, s := range secs {
		buf.WriteString(marker(s.name))
		endLine(&buf, s.req)
		endLine(&buf, s.resp)
	}
	return os.WriteFile(path, buf.Bytes(), mode)
}

// endLine writes b and ends the line it leaves open, if any. What follows is
// a marker, and a marker is a line of its own.
func endLine(buf *bytes.Buffer, b []byte) {
	if len(b) == 0 {
		return
	}
	buf.Write(b)
	if b[len(b)-1] != '\n' {
		buf.WriteByte('\n')
	}
}
