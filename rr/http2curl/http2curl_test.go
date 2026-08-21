package http2curl

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestCommand(t *testing.T) {
	tests := []struct {
		name   string
		method string
		url    string
		host   string
		header http.Header
		body   string
		want   string
	}{
		{
			name:   "nothing but a method and a URL",
			method: "GET",
			url:    "https://x/",
			want:   "curl -X GET https://x/\n",
		},
		{
			// The body arrives as httpfmt leaves it, newlines and all,
			// which is why it goes out as --data-raw and not as -d.
			name:   "an indented JSON body",
			method: "POST",
			url:    "https://x/items",
			header: http.Header{"Content-Type": {"application/json"}},
			body: `{
  "name": "x"
}`,
			want: `curl -X POST https://x/items \
  -H 'Content-Type: application/json' \
  --data-raw '{
  "name": "x"
}'
`,
		},
		{
			// A single quote cannot be written inside single quotes, so it
			// is written by leaving them and coming back.
			name:   "a value holding a single quote",
			method: "GET",
			url:    "https://x/",
			header: http.Header{"X-Note": {"it's fine"}},
			want: `curl -X GET https://x/ \
  -H 'X-Note: it'\''s fine'
`,
		},
		{
			name:   "a query is quoted for its ampersand",
			method: "GET",
			url:    "https://x/s?a=1&b=2",
			want:   "curl -X GET 'https://x/s?a=1&b=2'\n",
		},
		{
			// RFC 9110, 5.3: field lines that share a name are ordered, and
			// sorting the names must not disturb the order within one.
			name:   "repeated names keep their order",
			method: "GET",
			url:    "https://x/",
			header: http.Header{"Accept": {"a", "b"}},
			want: `curl -X GET https://x/ \
  -H 'Accept: a' \
  -H 'Accept: b'
`,
		},
		{
			name:   "names come out sorted",
			method: "GET",
			url:    "https://x/",
			header: http.Header{"Zeta": {"1"}, "Alpha": {"2"}},
			want: `curl -X GET https://x/ \
  -H 'Alpha: 2' \
  -H 'Zeta: 1'
`,
		},
		{
			// Curl reads X-Empty: as an order to drop the header; the
			// semicolon is how it is told to send it empty instead.
			name:   "an empty value is sent with a semicolon",
			method: "GET",
			url:    "https://x/",
			header: http.Header{"X-Empty": {""}},
			want: `curl -X GET https://x/ \
  -H 'X-Empty;'
`,
		},
		{
			name:   "a host apart from the URL",
			method: "GET",
			url:    "https://x/",
			host:   "y",
			want: `curl -X GET https://x/ \
  -H 'Host: y'
`,
		},
		{
			name:   "framing headers are left to curl",
			method: "POST",
			url:    "https://x/",
			header: http.Header{
				"Accept":            {"*/*"},
				"Content-Length":    {"3"},
				"Transfer-Encoding": {"chunked"},
			},
			body: "abc",
			want: `curl -X POST https://x/ \
  -H 'Accept: */*' \
  --data-raw abc
`,
		},
		{
			name:   "a body that is not text",
			method: "POST",
			url:    "https://x/",
			header: http.Header{"Content-Type": {"image/png"}},
			body:   "\x89PNG\x00\x1a",
			want: `curl -X POST https://x/ \
  -H 'Content-Type: image/png' \
  --data-binary @- # body is not text
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.Reader
			if tt.body != "" {
				body = bytes.NewReader([]byte(tt.body))
			}
			req, err := http.NewRequest(tt.method, tt.url, body)
			if err != nil {
				t.Fatal(err)
			}
			if tt.header != nil {
				req.Header = tt.header
			}
			if tt.host != "" {
				req.Host = tt.host
			}

			got, err := Command(req)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("command:\n%q\nwant:\n%q", got, tt.want)
			}
			// Writing the same request again must say the same thing: a
			// request read for its body has to be left able to send it.
			var b strings.Builder
			if err := Write(&b, req); err != nil {
				t.Fatal(err)
			}
			if b.String() != got {
				t.Fatalf("write:\n%q\ncommand:\n%q", b.String(), got)
			}
		})
	}
}

func TestCommandKeepsBody(t *testing.T) {
	// A body the request cannot rewind for itself is read and put back, so
	// the caller is left holding the request it had.
	req, err := http.NewRequest("POST", "https://x/", io.NopCloser(strings.NewReader("hello")))
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("want a request without GetBody")
	}
	if _, err := Command(req); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("body: %q, want %q", b, "hello")
	}
}

func TestQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"abc", "abc"},
		{"GET", "GET"},
		{"https://x/p?a=1", "'https://x/p?a=1'"},
		{"", "''"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
		{"~/x", "'~/x'"},
		{"a&b", "'a&b'"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := quote(tt.in); got != tt.want {
				t.Fatalf("quote: %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuoteAgainstBash(t *testing.T) {
	// The point of quoting is what bash makes of it, so ask bash rather
	// than trust the table above.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	words := []string{
		"", "abc", "a b", "it's", "$HOME", "${TOKEN}", "~/x", "a&b",
		"`id`", "a\nb", `a\b`, "*", "a|b", `"q"`, "a;b", "#c", "(p)",
		"{\n  \"name\": \"x\"\n}",
	}
	for _, s := range words {
		out, err := exec.Command(bash, "-c", "printf %s "+quote(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("bash read %s as %q, want %q", quote(s), out, s)
		}
	}
}
