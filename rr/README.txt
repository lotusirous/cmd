Rr sends the HTTP request stored in a file and writes the response back into
the same file, after a ---- line. The name is those two halves: request and
response.

	go install github.com/lotusirous/cmd/rr@latest

Usage:

	rr run file	send the request in file, store the response in it
	rr fmt file	format file

The request is wire-format HTTP with an absolute request URI, so there is
nothing to learn and nothing to export. Everything after the blank line is the
body; rr measures it, and Content-Length need be neither written by hand nor
kept up to date. ${NAME} and $NAME are read from the environment when the
request is sent, in the body as well as the headers, and the file keeps the
unexpanded text, so it is safe to commit.

	$ TOKEN=$(pass show api/token) rr run items.rr
	$ cat items.rr
	POST https://api.example.com/items HTTP/1.1
	Content-Type: application/json
	Authorization: Bearer ${TOKEN}

	{
	  "name": "x"
	}
	----
	HTTP/1.1 201 Created
	Content-Length: 28
	Content-Type: application/json
	Date: Mon, 01 Jan 2035 00:00:00 GMT

	{
	  "id": 7,
	  "name": "x"
	}

Everything above the ---- line is what you wrote, in the form rr stores it.
Running formats the request first: a known method is upper-cased, a standard
header name is respelled and written ahead of the custom ones, which are left
alone, and a JSON body is indented when Content-Type says it is JSON. Rr fmt
does that formatting on its own. A failed request leaves the file untouched.

The response is stored as it came off the wire. Redirects are not followed, so
it is the answer to this request rather than the one at the end of a chain.

A request and its last answer being one text file is the point of the tool.
A directory of files is a collection, the environment is the environment,
sharing is git push, and git diff after a run reports what an API changed.

	for f in api/*.rr; do rr run "$f" >/dev/null; done
	git diff --exit-code api/

Rr runs one request per file. It has no cookie jar, no OAuth flow, and no
assertions. A multipart body is sent as written, boundaries and all, but there
is no helper to compose one, and no way to attach a file that is not text. A
body holding a line that is exactly ---- is cut short there, that line being
what separates the two halves. A request over TLS may negotiate HTTP/2 and be
stored as such. Each request gets thirty seconds, and ^C cancels one in
flight.
