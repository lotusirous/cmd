package main

// The rr file itself: the markers that open an exchange, the two halves each
// one holds, and the reading, parsing and writing of the text on disk. What
// parse reads, rewriteFile writes.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// The two ends of a marker, the line that opens an exchange. A line that
// begins with one, ends with the other, and has room for a name between is a
// marker; every other line is text, so a body line of dashes stays a body
// line.
var (
	marker    = []byte("-- ")
	markerEnd = []byte(" --")
)

// An exchange is what a marker line opens and names: the request as the file
// has it, and the response last stored under it, empty until the request has
// been sent. It is the two halves the tool is named for.
type exchange struct {
	name string
	req  []byte
	resp []byte
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

// parse returns the text before the first marker line, which is a comment,
// and the exchanges the markers open. A name has to be there and has to be its
// own: it is what -match selects and what an error quotes, so two exchanges
// answering to one name is a file written wrong.
func parse(data []byte) (head []byte, exchanges []exchange, err error) {
	seen := make(map[string]bool)
	head = data
	name := ""
	start, off := 0, 0
	for line := range bytes.Lines(data) {
		next := off + len(line)
		mark, ok := markerName(bytes.TrimRight(line, "\r\n"))
		if !ok {
			off = next
			continue
		}
		if name == "" {
			head = data[:off] // nothing is open yet: this opens the first
		} else {
			exchanges = append(exchanges, cut(name, data[start:off]))
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
		exchanges = append(exchanges, cut(name, data[start:]))
	}
	if len(exchanges) == 0 {
		return nil, nil, errors.New("no exchange")
	}
	for _, s := range exchanges {
		if len(bytes.TrimSpace(s.req)) == 0 {
			return nil, nil, fmt.Errorf("exchange %s has no request", s.name)
		}
	}
	return head, exchanges, nil
}

// cut returns the exchange that name and text hold, dividing text at the line
// that begins the stored response. One that has never been sent has no such
// line, and no response.
func cut(name string, text []byte) exchange {
	off := 0
	for line := range bytes.Lines(text) {
		if isStatus(bytes.TrimRight(line, "\r\n")) {
			return exchange{name, text[:off], text[off:]}
		}
		off += len(line)
	}
	return exchange{name: name, req: text}
}

// markerName returns the name a marker line holds, and whether line is one.
// The second cut is what keeps a line too short from being one: "-- --"
// begins and ends with the same three bytes, and what the first cut leaves
// has no room for the second.
func markerName(line []byte) (string, bool) {
	rest, ok := bytes.CutPrefix(line, marker)
	if !ok {
		return "", false
	}
	rest, ok = bytes.CutSuffix(rest, markerEnd)
	if !ok {
		return "", false
	}
	return string(bytes.TrimSpace(rest)), true
}

// markerLine returns the line that opens the exchange named name.
func markerLine(name string) string {
	return string(marker) + name + string(markerEnd) + "\n"
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

// rewriteFile stores head and exchanges in path. Writing in place keeps the mode,
// the owner, and any link or symbolic link to the file; mode applies only if
// it has gone missing since it was read. A write that fails midway leaves the
// file truncated, which is the price of not renaming a temporary over it: the
// rename replaces the file, and with it everything the file system knew about
// the one the user made.
func rewriteFile(path string, mode os.FileMode, head []byte, exchanges []exchange) error {
	var buf bytes.Buffer
	endLine(&buf, head)
	for _, s := range exchanges {
		buf.WriteString(markerLine(s.name))
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
