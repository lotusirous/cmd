package httpfmt

import "testing"

func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "header names and spacing",
			in:   "GET https://x/ HTTP/1.1\ncontent-type:application/json\nauthorization:  Bearer x \n",
			want: "GET https://x/ HTTP/1.1\nContent-Type: application/json\nAuthorization: Bearer x\n\n",
		},
		{
			// A standard name is respelled; a custom one is its author's,
			// and country-id must not become Country-Id.
			name: "a custom name keeps its casing",
			in:   "GET https://x/ HTTP/1.1\nhost: h\ncountry-id: vn\nX-API-KEY: k\n",
			want: "GET https://x/ HTTP/1.1\nHost: h\ncountry-id: vn\nX-API-KEY: k\n\n",
		},
		{
			name: "an empty value keeps no trailing space",
			in:   "GET https://x/ HTTP/1.1\nX-Empty:\t\n",
			want: "GET https://x/ HTTP/1.1\nX-Empty:\n\n",
		},
		{
			// The first blank line ends the header block, so a blank line
			// inside the body is just part of the body.
			name: "a blank line in the body",
			in:   "POST https://x/ HTTP/1.1\n\na\n\nb",
			want: "POST https://x/ HTTP/1.1\n\na\n\nb\n",
		},
		{
			// Line folding is obsolete, so a continuation is unfolded.
			name: "a folded header is unfolded",
			in:   "GET https://x/ HTTP/1.1\nX-Long: a\n  b\n",
			want: "GET https://x/ HTTP/1.1\nX-Long: a b\n\n",
		},
		{
			name: "crlf becomes lf",
			in:   "GET https://x/ HTTP/1.1\r\nHost: h\r\n\r\n",
			want: "GET https://x/ HTTP/1.1\nHost: h\n\n",
		},
		{
			// rr computes the framing, so carrying it in the file is noise
			// that goes stale the moment the body is edited.
			name: "framing headers are dropped",
			in:   "POST https://x/ HTTP/1.1\nContent-Length: 2\nTransfer-Encoding: chunked\nAccept: */*\n\nhello\n",
			want: "POST https://x/ HTTP/1.1\nAccept: */*\n\nhello\n",
		},
		{
			name: "a declared json body is indented",
			in:   "POST https://x/ HTTP/1.1\nContent-Type: application/json\n\n{\"a\":1}",
			want: "POST https://x/ HTTP/1.1\nContent-Type: application/json\n\n{\n  \"a\": 1\n}\n",
		},
		{
			name: "a +json type is indented too",
			in:   "POST https://x/ HTTP/1.1\nContent-Type: application/vnd.api+json; charset=utf-8\n\n{\"a\":1}",
			want: "POST https://x/ HTTP/1.1\nContent-Type: application/vnd.api+json; charset=utf-8\n\n{\n  \"a\": 1\n}\n",
		},
		{
			// Two Content-Type lines are malformed, but http.Header.Get
			// reads the first, so the body here is text and stays as it is.
			name: "a repeated content type is read once",
			in:   "POST https://x/ HTTP/1.1\nContent-Type: text/plain\nContent-Type: application/json\n\n{\"a\":1}",
			want: "POST https://x/ HTTP/1.1\nContent-Type: text/plain\nContent-Type: application/json\n\n{\"a\":1}\n",
		},
		{
			// A body that merely parses as JSON is not ours to reformat.
			name: "an undeclared json body is left alone",
			in:   "POST https://x/ HTTP/1.1\n\n{\"a\":1}",
			want: "POST https://x/ HTTP/1.1\n\n{\"a\":1}\n",
		},
		{
			name: "a body that is not json is left alone",
			in:   "POST https://x/ HTTP/1.1\n\nname=x&y=2",
			want: "POST https://x/ HTTP/1.1\n\nname=x&y=2\n",
		},
		{
			name: "the header block is terminated",
			in:   "GET https://x/ HTTP/1.1\nHost: h",
			want: "GET https://x/ HTTP/1.1\nHost: h\n\n",
		},
		{
			name: "custom names keep their order",
			in:   "GET https://x/ HTTP/1.1\nZ: 1\nA: 2\nM: 3\n\n",
			want: "GET https://x/ HTTP/1.1\nZ: 1\nA: 2\nM: 3\n\n",
		},
		{
			// The standard names lead, and each group keeps the order the
			// file has it in.
			name: "standard names come first",
			in:   "GET https://x/ HTTP/1.1\ncountry-id: vn\nHost: h\nX-Key: k\nAccept: */*\n",
			want: "GET https://x/ HTTP/1.1\nHost: h\nAccept: */*\ncountry-id: vn\nX-Key: k\n\n",
		},
		{
			// RFC 9110, 5.3: field lines that share a name are ordered, so
			// partitioning must not shuffle them.
			name: "repeated names keep their order",
			in:   "GET https://x/ HTTP/1.1\nAccept: a\nX-Tag: 1\nAccept: b\nX-Tag: 2\n",
			want: "GET https://x/ HTTP/1.1\nAccept: a\nAccept: b\nX-Tag: 1\nX-Tag: 2\n\n",
		},
		{
			name: "a variable is not expanded",
			in:   "GET https://x/ HTTP/1.1\nauthorization: Bearer ${TOKEN}\n",
			want: "GET https://x/ HTTP/1.1\nAuthorization: Bearer ${TOKEN}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Format([]byte(tt.in)))
			if got != tt.want {
				t.Fatalf("format:\n%q\nwant:\n%q", got, tt.want)
			}
			if again := string(Format([]byte(got))); again != got {
				t.Fatalf("not idempotent:\n%q\nthen:\n%q", got, again)
			}
		})
	}
}
