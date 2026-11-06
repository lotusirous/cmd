package main

import (
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty"},
		{name: "url", in: "https://example.com/foo bar", want: "https%3A%2F%2Fexample.com%2Ffoo%20bar"},
		{name: "plus", in: "a+b c", want: "a%2Bb%20c"},
		{name: "unreserved", in: "AZaz09-_.~", want: "AZaz09-_.~"},
		{name: "reserved", in: "!#$&'()*,/:;=?@[]", want: "%21%23%24%26%27%28%29%2A%2C%2F%3A%3B%3D%3F%40%5B%5D"},
		{name: "percent", in: "100%", want: "100%25"},
		{name: "utf8", in: "héllo", want: "h%C3%A9llo"},
		{name: "raw", in: "caf\xe9", want: "caf%E9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := encode(tt.in)
			if got != tt.want {
				t.Fatalf("encode(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Every encoding must decode back to its input.
			if back := decode(got); back != tt.in {
				t.Fatalf("decode(%q) = %q, want %q", got, back, tt.in)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty"},
		{name: "space", in: "a%20b+c", want: "a b c"},
		{name: "plus", in: "a%2Bb", want: "a+b"},
		{name: "lower", in: "%c3%a9", want: "é"},
		{name: "raw", in: "caf%E9", want: "caf\xe9"},
		{name: "newline", in: "a%0Ab", want: "a\nb"},
		{name: "bad", in: "%zz", want: "%zz"},
		{name: "half", in: "%4g", want: "%4g"},
		{name: "short", in: "%4", want: "%4"},
		{name: "end", in: "100%", want: "100%"},
		{name: "sign", in: "%+1", want: "% 1"},
		{name: "twice", in: "%%41", want: "%A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decode(tt.in); got != tt.want {
				t.Fatalf("decode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLines(t *testing.T) {
	tests := []struct {
		name string
		f    func(string) string
		in   string
		want string
	}{
		{name: "empty", f: encode},
		{name: "blank", f: encode, in: "\n", want: "\n"},
		{name: "each", f: encode, in: "a b\nc/d\n", want: "a%20b\nc%2Fd\n"},
		{name: "unterminated", f: encode, in: "a b", want: "a%20b\n"},
		{name: "crlf", f: encode, in: "a b\r\n", want: "a%20b\n"},
		{name: "long", f: encode, in: strings.Repeat("a", 1<<20), want: strings.Repeat("a", 1<<20) + "\n"},
		{name: "decode", f: decode, in: "a%20b+c\n50%\n", want: "a b c\n50%\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := lines(strings.NewReader(tt.in), &b, tt.f); err != nil {
				t.Fatal(err)
			}
			if got := b.String(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
