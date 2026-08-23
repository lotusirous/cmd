package main

// The file grammar. A case file cannot show it: Parse and text are inverse,
// so a response mistaken for a request writes itself back out the same.

import (
	"strings"
	"testing"
)

func TestMarkerName(t *testing.T) {
	for _, tt := range []struct {
		line string
		name string
		ok   bool
	}{
		{"-- a --", "a", true},
		{"--   spaced name   --", "spaced name", true},
		{"--  --", "", true}, // room for a name, and none in it
		{"-- --", "", false}, // too short to be one: the cuts would overlap
		{"----", "", false},
		{"-- a", "", false},
		{"a --", "", false},
		{"GET https://x/-- a --", "", false}, // a marker is a line, not a word
	} {
		name, ok := markerName([]byte(tt.line))
		if name != tt.name || ok != tt.ok {
			t.Errorf("markerName(%q) = %q, %v, want %q, %v", tt.line, name, ok, tt.name, tt.ok)
		}
	}
}

func TestIsStatus(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{"HTTP/1.1 200 OK", true},
		{"HTTP/2.0 204 No Content", true}, // the version is read, not assumed
		{"HTTP/1.1 200", true},
		{"HTTP/11 200 OK", false}, // no dot, so no version
		{"HTTP/1.a 200 OK", false},
		{"HTTP/1.1 20 OK", false}, // a status code is three digits
		{"HTTP/1.1", false},
		{"X-Note: HTTP/1.1 is fine", false},
	} {
		if got := isStatus([]byte(tt.line)); got != tt.want {
			t.Errorf("isStatus(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	const file = "what this file is for\n\n" +
		"--   a   --\nGET https://x/\n----\n\nHTTP/1.1 200 OK\n\nhi\n" +
		"-- b --\nGET https://x/-- c --\n-- --\n"

	f, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.Comment); got != "what this file is for\n\n" {
		t.Errorf("comment = %q", got)
	}

	type half struct{ name, request, response string }
	want := []half{
		// Above the status line is the request, "----" among it.
		{"a", "GET https://x/\n----\n\n", "HTTP/1.1 200 OK\n\nhi\n"},
		// Neither "-- --" nor a marker inside a line opens an exchange.
		{"b", "GET https://x/-- c --\n-- --\n", ""},
	}
	if len(f.Exchanges) != len(want) {
		t.Fatalf("%d exchanges, want %d", len(f.Exchanges), len(want))
	}
	for i, ex := range f.Exchanges {
		got := half{ex.Name, string(ex.Request), string(ex.Response)}
		if got != want[i] {
			t.Errorf("exchange %d = %q, want %q", i, got, want[i])
		}
	}

	// text writes back what Parse read, the marker spacing made canonical.
	canon := strings.Replace(file, "--   a   --", "-- a --", 1)
	if got := string(text(&f)); got != canon {
		t.Errorf("text:\n%q\nwant:\n%q", got, canon)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"GET https://x/ HTTP/1.1\n", "no exchange"},
		{"--  --\nGET https://x/\n", "exchange with no name"},
		{"-- a --\nGET https://x/\n-- a --\nGET https://y/\n", "two exchanges named a"},
		{"-- a --\n", "exchange a has no request"},
		{"-- a --\nHTTP/1.1 200 OK\n", "exchange a has no request"},
	} {
		if _, err := Parse([]byte(tt.in)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%q) = %v, want it to say %q", tt.in, err, tt.want)
		}
	}
}
