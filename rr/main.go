// Rr sends the HTTP request stored in a file and writes the response back
// into the same file, after a ---- line.
//
// Usage:
//
//	rr file
//
// The request is wire-format HTTP with an absolute-form request URI.
// ${NAME} and $NAME are expanded from the environment before sending, but
// the file on disk keeps the unexpanded text, so it stays safe to commit.
// JSON response bodies are indented.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const delim = "----"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rr file")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirect,
	}
	if err := run(ctx, os.Args[1], client, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "rr: %v\n", err)
		os.Exit(1)
	}
}

// noRedirect keeps the client from following redirects, so the stored
// response is the one the file's request produced.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// run executes the HTTP request in file and stores the response after ----.
// Errors name the file exactly once: those that already carry it, such as
// *fs.PathError, are returned as they are.
func run(ctx context.Context, path string, client *http.Client, w io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	req := splitFile(data)
	expanded, err := expandEnv(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	hreq, err := parseRequest(ctx, expanded)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	resp, err := client.Do(hreq)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	wire, err := responseWire(resp)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := w.Write(wire); err != nil {
		return err
	}
	return rewriteFile(path, info.Mode().Perm(), req, wire)
}

// splitFile returns the request bytes before the first ---- line.
func splitFile(data []byte) []byte {
	for off := 0; off < len(data); {
		line, _, _ := bytes.Cut(data[off:], []byte("\n"))
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte(delim)) {
			return data[:off]
		}
		off += len(line) + 1
	}
	return data
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
func parseRequest(ctx context.Context, b []byte) (*http.Request, error) {
	in, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		return nil, err
	}
	if in.URL.Scheme != "http" && in.URL.Scheme != "https" {
		return nil, errors.New("request URI must be absolute (https://... or http://...)")
	}
	body, err := io.ReadAll(in.Body)
	in.Body.Close()
	if err != nil {
		return nil, err
	}
	// ReadRequest yields a server-side request, which a client cannot send;
	// rebuild it from the URL and copy over what the file asked for.
	out, err := http.NewRequestWithContext(ctx, in.Method, in.URL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header = in.Header.Clone()
	if in.Host != "" {
		out.Host = in.Host
	}
	return out, nil
}

// responseWire reads and closes resp's body and returns the response in
// wire format, indenting JSON bodies.
func responseWire(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body = formatBody(body)

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

// formatBody pretty-prints JSON with two-space indent.
func formatBody(body []byte) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(body), "", "  "); err != nil {
		return body
	}
	return out.Bytes()
}

// rewriteFile stores req, ----, and the response in path, keeping mode.
// The replacement is atomic: the contents are flushed before the rename,
// so an interrupted run leaves the old file untouched.
func rewriteFile(path string, mode os.FileMode, req, resp []byte) (err error) {
	var buf bytes.Buffer
	buf.Write(req)
	if len(req) > 0 && req[len(req)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString(delim + "\n")
	buf.Write(resp)

	tmp, err := os.CreateTemp(filepath.Dir(path), ".rr-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
