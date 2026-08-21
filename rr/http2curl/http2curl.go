// Package http2curl writes an HTTP request as a curl command.
//
// A request a program holds is easy to send and hard to hand to anyone else.
// [Command] writes one as the curl command that sends it, quoted for bash, so
// it can be pasted into a shell, a bug report, or a message to whoever runs
// the server. It is the only thing this package does.
//
// The command says what the request says and no more: no -s, no -i, no -L,
// no --compressed. Header names come out in the canonical spelling, X-API-KEY
// as X-Api-Key, because [http.Header] canonicalized them long before this
// package saw them, and they come out sorted, because a header map has no
// order left to keep. Field lines repeating a name keep their order among
// themselves, which is the order RFC 9110, 5.3 gives meaning to.
package http2curl

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

// Command returns the curl command that sends req.
func Command(req *http.Request) (string, error) {
	var b strings.Builder
	if err := Write(&b, req); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Write writes to w the curl command that sends req.
func Write(w io.Writer, req *http.Request) error {
	l, err := lines(req)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, strings.Join(l, " \\\n  ")+"\n")
	return err
}

// lines returns the command a line to an element. The program, the method,
// and the URL share the first line, then a header to a line, then the body.
// Joining them is all the layout there is, so a request with nothing but a
// URL comes out on one line without being a case of its own.
func lines(req *http.Request) ([]string, error) {
	l := []string{"curl -X " + quote(req.Method) + " " + quote(req.URL.String())}

	// A request carries its host apart from its URL, and curl takes the
	// URL's unless the command says otherwise.
	if req.Host != "" && req.Host != req.URL.Host {
		l = append(l, header("Host", req.Host))
	}
	for _, name := range slices.Sorted(maps.Keys(req.Header)) {
		switch name {
		case "Content-Length", "Transfer-Encoding":
			continue // curl frames the body it is handed
		}
		for _, v := range req.Header[name] {
			l = append(l, header(name, v))
		}
	}

	b, err := readBody(req)
	if err != nil {
		return nil, err
	}
	switch {
	case len(b) == 0:
	case text(b):
		// --data-raw and not -d: -d strips the newlines an indented JSON
		// body is written with, and reads a leading @ as a file name.
		l = append(l, "--data-raw "+quote(string(b)))
	default:
		// A body that is not text is no shell word. The command reads it
		// from standard input instead, and says so rather than pretend it
		// runs as it stands.
		l = append(l, "--data-binary @- # body is not text")
	}
	return l, nil
}

// header returns the -H argument for one field line. Curl reads a trailing
// colon as an instruction to drop a header it would otherwise send, so a
// field written with no value takes the semicolon curl provides for saying
// that the field is sent and empty.
func header(name, value string) string {
	if value == "" {
		return "-H " + quote(name+";")
	}
	return "-H " + quote(name+": "+value)
}

// readBody returns the body of req and leaves req able to send it. A request
// built by [http.NewRequest] over a buffer has GetBody, which hands out a
// fresh reader; one built any other way has its Body read and put back.
func readBody(req *http.Request) ([]byte, error) {
	switch {
	case req.GetBody != nil:
		r, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case req.Body != nil:
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
		return b, err
	}
	return nil, nil
}

// text reports whether b can be written as a shell word. A shell word is a C
// string, so a NUL ends it early, and a byte that is not UTF-8 arrives at the
// far end as whatever the reader's encoding makes of it.
func text(b []byte) bool {
	return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

// safe holds the bytes a bash word may hold unquoted. Tilde is not among
// them, bash expanding a leading one, and neither is anything above ASCII.
const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@%_+=:,./-"

// quote returns s as one bash word. A word needing no quotes keeps none;
// anything else is single-quoted, single quotes being the only ones bash does
// nothing inside of. The single quote itself cannot be written there, so it
// is written by leaving the quotes, escaping it, and going back in.
func quote(s string) string {
	if s != "" && bare(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// bare reports whether s is a bash word that needs no quoting.
func bare(s string) bool {
	for i := range len(s) {
		if strings.IndexByte(safe, s[i]) < 0 {
			return false
		}
	}
	return true
}
