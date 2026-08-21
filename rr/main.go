/*
Rr sends the HTTP request stored in a file and writes the response back into
the same file, after a ---- line. The name is those two halves: request and
response.

Usage:

	rr run file
	rr fmt file

Rr run sends the request and stores the response. Rr fmt only formats the
file, sending nothing and leaving any stored response alone.

The request is wire-format HTTP with an absolute-form request URI. It is put
in canonical form before it is sent, so a file written by hand ends up stored
the way rr would have written it.

${NAME} and $NAME are expanded from the environment before sending, but the
file on disk keeps the unexpanded text, so it stays safe to commit.

Everything after the blank line is sent as the body, less the newline that
ends the file; write two to send a body that ends in one. Rr frames the
request itself, so Content-Length need not be written by hand, nor kept up to
date when the body changes. A body is indented when its Content-Type says it
is JSON, in the request as in the response.
*/
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/lotusirous/cmd/rr/httpfmt"
)

const delim = "----"

const usageText = `usage: rr run file	send the request in file, store the response in it
       rr fmt file	format file
file is a wire-format HTTP request:
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

	args := os.Args[1:]
	if len(args) != 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch path := args[1]; args[0] {
	case "run":
		client := &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: noRedirect,
		}
		err = run(ctx, path, client, os.Stdout)
	case "fmt":
		err = formatFile(path)
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

// noRedirect keeps the client from following redirects, so the stored
// response is the one the file's request produced.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// run executes the HTTP request in file and stores the response after ----.
// Every error it returns is an *fs.PathError naming the file, either from the
// os call that failed or made here, so all of them read alike.
func run(ctx context.Context, path string, client *http.Client, w io.Writer) error {
	data, mode, err := readFile(path)
	if err != nil {
		return err
	}

	src, _ := splitFile(data)
	src = httpfmt.Format(src)
	expanded, err := expandEnv(src)
	if err != nil {
		return &fs.PathError{Op: "expand", Path: path, Err: err}
	}

	req, err := parseRequest(ctx, expanded)
	if err != nil {
		return &fs.PathError{Op: "parse", Path: path, Err: err}
	}

	resp, err := client.Do(req)
	if err != nil {
		return &fs.PathError{Op: "send", Path: path, Err: err}
	}

	wire, err := responseWire(resp)
	if err != nil {
		return &fs.PathError{Op: "read", Path: path, Err: err}
	}
	if _, err := w.Write(wire); err != nil {
		return err
	}
	return rewriteFile(path, mode, src, wire)
}

// formatFile rewrites the request half of path in canonical form and sends
// nothing. Whatever follows the ---- line is left exactly as it was.
func formatFile(path string) error {
	data, mode, err := readFile(path)
	if err != nil {
		return err
	}
	src, rest := splitFile(data)
	return os.WriteFile(path, append(httpfmt.Format(src), rest...), mode)
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

// splitFile returns the request bytes before the first ---- line, and the
// rest of the file from that line on.
func splitFile(data []byte) (req, rest []byte) {
	for off := 0; off < len(data); {
		line, _, _ := bytes.Cut(data[off:], []byte("\n"))
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte(delim)) {
			return data[:off], data[off:]
		}
		off += len(line) + 1
	}
	return data, nil
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
			return nil, errors.New("empty file")
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
// wire format, indenting JSON bodies.
func responseWire(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if isJSON(resp.Header.Get("Content-Type")) {
		body = indentJSON(body)
	}

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

// rewriteFile stores req, ----, and the response in path. Writing in place
// keeps the mode, the owner, and any link or symbolic link to the file; mode
// applies only if it has gone missing since it was read. A write that fails
// midway leaves the file truncated, which is the price of not renaming a
// temporary over it: the rename replaces the file, and with it everything the
// file system knew about the one the user made.
func rewriteFile(path string, mode os.FileMode, req, resp []byte) error {
	var buf bytes.Buffer
	buf.Write(req)
	if len(req) > 0 && req[len(req)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString(delim + "\n")
	buf.Write(resp)
	return os.WriteFile(path, buf.Bytes(), mode)
}
