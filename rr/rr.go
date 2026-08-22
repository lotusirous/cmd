package main

// The rr file itself: the markers that open an exchange, the two halves each
// one holds, and the reading, parsing and writing of the text on disk. What
// Parse reads, text writes.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/lotusirous/cmd/rr/httpfmt"
)

var (
	marker    = []byte("-- ")
	markerEnd = []byte(" --")
)

// A File is a collection of exchanges.
type File struct {
	Comment   []byte
	Exchanges []Exchange
}

// An Exchange is what a marker line opens and names: the request as the file
// has it, and the response last stored under it, empty until the request has
// been sent. It is the two halves the tool is named for.
type Exchange struct {
	name string
	req  []byte
	resp []byte
}

// text returns the text f is kept as: the comment, then each exchange under
// the marker line naming it, request first and stored response after. A part
// that does not end in \n has one added.
func text(f *File) []byte {
	var buf bytes.Buffer
	endLine(&buf, f.Comment)
	for _, ex := range f.Exchanges {
		buf.WriteString(markerLine(ex.name))
		endLine(&buf, ex.req)
		endLine(&buf, ex.resp)
	}
	return buf.Bytes()
}

// Format returns the text of f with the request of every exchange in canonical
// form, as [httpfmt.Format] writes one. The comment and the stored responses
// are returned unchanged, and f is not modified.
func Format(f *File) []byte {
	c := *f
	c.Exchanges = slices.Clone(f.Exchanges)
	for i := range c.Exchanges {
		c.Exchanges[i].req = httpfmt.Format(c.Exchanges[i].req)
	}
	return text(&c)
}

// readFile returns the contents of path and the permission bits of its mode,
// which is the mode to store it back under.
func readFile(path string) ([]byte, os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), err
}

// Parse returns the [File] that data holds: the text before the first marker
// line as the comment, and one [Exchange] for each marker line after it. Parse
// returns an error if data holds no exchange, or an exchange with no name, a
// name another exchange already has, or no request.
func Parse(data []byte) (File, error) {
	seen := make(map[string]bool)
	var exchanges []Exchange
	head := data
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
			return File{}, errors.New("exchange with no name")
		case seen[mark]:
			return File{}, fmt.Errorf("two exchanges named %s", mark)
		}
		seen[mark] = true
		name, start, off = mark, next, next
	}
	if name != "" {
		exchanges = append(exchanges, cut(name, data[start:]))
	}
	if len(exchanges) == 0 {
		return File{}, errors.New("no exchange")
	}
	for _, s := range exchanges {
		if len(bytes.TrimSpace(s.req)) == 0 {
			return File{}, fmt.Errorf("exchange %s has no request", s.name)
		}
	}
	return File{Comment: head, Exchanges: exchanges}, nil
}

// If text holds a line that begins a stored response, cut returns the
// [Exchange] named name with the request before that line and the response
// from it on. Otherwise, cut returns the Exchange with all of text as the
// request and no response.
func cut(name string, text []byte) Exchange {
	off := 0
	for line := range bytes.Lines(text) {
		if isStatus(bytes.TrimRight(line, "\r\n")) {
			return Exchange{name, text[:off], text[off:]}
		}
		off += len(line)
	}
	return Exchange{name: name, req: text}
}

// If line begins with "-- ", ends with " --", and has room for a name between
// them, markerName returns that name with surrounding spaces removed, and
// true. Otherwise markerName returns "" and false.
func markerName(line []byte) (string, bool) {
	rest, ok := bytes.CutPrefix(line, marker)
	if !ok {
		return "", false
	}
	// "-- --" is too short to be one: the two cuts would overlap.
	rest, ok = bytes.CutSuffix(rest, markerEnd)
	if !ok {
		return "", false
	}
	return string(bytes.TrimSpace(rest)), true
}

// markerLine returns the line that opens the exchange named name, ending in \n.
func markerLine(name string) string {
	return string(marker) + name + string(markerEnd) + "\n"
}

// isStatus reports whether line is a status line: HTTP/, a version of two
// runs of digits separated by a dot, and a three-digit status code.
func isStatus(line []byte) bool {
	version, rest, ok := bytes.Cut(line, []byte(" "))
	if !ok || !bytes.HasPrefix(version, []byte("HTTP/")) {
		return false
	}
	// The version is read, not assumed: TLS may negotiate HTTP/2.
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

// If b is empty, endLine writes nothing.
// Otherwise, endLine writes b, followed by a \n if b does not end in one.
func endLine(buf *bytes.Buffer, b []byte) {
	if len(b) == 0 {
		return
	}
	buf.Write(b)
	if b[len(b)-1] != '\n' {
		buf.WriteByte('\n') // a marker is a line of its own
	}
}
