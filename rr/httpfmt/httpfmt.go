// Package httpfmt formats HTTP requests written by hand.
//
// An HTTP request is text, but wire format is unforgiving text: the header
// block ends with a blank line, header names are canonical, and a body is
// framed by a length that has to be counted. [Format] puts a request written
// by hand into that shape, leaving the request itself alone. It is the only
// thing this package does.
package httpfmt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/textproto"
	"strings"
)

// Format returns req in canonical form: a standard header name respelled the
// standard way, one space after each header colon, no framing headers, a
// blank line ending the header block, and an indented body when Content-Type
// declares JSON. It is a
// rewrite of the text, not of the request: header order is kept, a body that
// is not JSON is left alone, and ${NAME} is left for the caller to expand.
//
// Framing headers are dropped because a length written by hand goes stale
// the moment the body is edited; the caller frames the body it sends.
func Format(req []byte) []byte {
	if len(bytes.TrimSpace(req)) == 0 {
		return nil // an empty file has no request to format
	}
	// Read the header block as net/http reads one, then take what is left as
	// the body. textproto ends the block at a blank line or at the end of the
	// text, so a file that stops after its last header needs no special case.
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(req)))

	first, err := tp.ReadLine()
	if err != nil {
		return req
	}
	var buf bytes.Buffer
	buf.WriteString(canonMethod(first))
	buf.WriteByte('\n')

	var contentType string
	for {
		line, err := tp.ReadContinuedLine()
		if err != nil || line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			buf.WriteString(line)
			buf.WriteByte('\n')
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		canon := textproto.CanonicalMIMEHeaderKey(name)
		switch {
		case canon == "Content-Length", canon == "Transfer-Encoding":
			continue
		case canon == "Content-Type":
			contentType = value
		}
		if std, ok := stdHeader[canon]; ok {
			name = std
		}
		if value == "" {
			fmt.Fprintf(&buf, "%s:\n", name) // no value, and so no trailing space
			continue
		}
		fmt.Fprintf(&buf, "%s: %s\n", name, value)
	}
	buf.WriteByte('\n')

	body, _ := io.ReadAll(tp.R)
	if isJSON(contentType) {
		body = indentJSON(body)
	}
	if body = bytes.TrimRight(body, "\r\n"); len(body) > 0 {
		buf.Write(body)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// stdHeader maps the canonical form of a standard request header to the way
// it is spelled. A name that is not here is written as the file has it: the
// casing of a custom header is its author's, not ours to correct.
//
// See: https://www.iana.org/assignments/http-fields/http-fields.xhtml
var stdHeader = map[string]string{}

func init() {
	for _, h := range []string{
		// RFC 9110, HTTP Semantics.
		"Accept",
		"Accept-Charset",
		"Accept-Encoding",
		"Accept-Language",
		"Authorization",
		"Connection",
		"Content-Encoding",
		"Content-Language",
		"Content-Length",
		"Content-Location",
		"Content-Type",
		"Date",
		"Expect",
		"From",
		"Host",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Range",
		"If-Unmodified-Since",
		"Max-Forwards",
		"Proxy-Authorization",
		"Range",
		"Referer",
		"TE",
		"Trailer",
		"Upgrade",
		"User-Agent",
		"Via",

		// Defined elsewhere, and as common in a request written by hand.
		"Cache-Control",       // RFC 9111
		"Content-Disposition", // RFC 6266
		"Cookie",              // RFC 6265
		"Forwarded",           // RFC 7239
		"Origin",              // RFC 6454
		"Pragma",              // RFC 9111
		"Transfer-Encoding",   // RFC 9112
	} {
		stdHeader[textproto.CanonicalMIMEHeaderKey(h)] = h
	}
}

// canonMethod uppercases a known method, which a request written by hand
// often has in lower case. A method is case-sensitive, so an unknown one is
// left exactly as written.
func canonMethod(line string) string {
	method, rest, ok := strings.Cut(line, " ")
	if !ok {
		return line
	}
	switch up := strings.ToUpper(method); up {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE":
		return up + " " + rest
	}
	return line
}

// isJSON reports whether a Content-Type declares a JSON body: application/json,
// or one of the +json structured syntax suffixes, with any parameters. A body
// is indented only when it says it is JSON, since one that merely parses as
// JSON is not ours to reformat.
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
