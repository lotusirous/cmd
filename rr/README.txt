Rr sends the HTTP requests stored in a file and writes each response back into
the same file, under the request that made it. The name is those two halves:
request and response.

	go install github.com/lotusirous/cmd/rr@latest

Usage:

	rr run [-match regexp] file	send the requests in file, store the responses in it
	rr fmt file	format file
	rr gen [-match regexp] [-form form] file	write the requests as curl commands

A file holds a collection of exchanges, each opened by a line naming it:

	-- items/create --

Text above the first such line is a comment rr does not read. A name is the
writer's own, and it is what -match selects on and what an error quotes, so no
two exchanges in a file may answer to one. Rr sends them in the order the file
has them and stops at the first failure, an exchange being free to rely on the
one above it.

A request is wire-format HTTP with an absolute request URI, so there is
nothing to learn and nothing to export. Everything after the blank line is the
body; rr measures it, and Content-Length need be neither written by hand nor
kept up to date. ${NAME} and $NAME are read from the environment when the
request is sent, in the body as well as the headers, and the file keeps the
unexpanded text, so it is safe to commit.

	$ TOKEN=$(pass show api/token) rr run items.rr
	$ cat items.rr
	-- items/create --
	POST https://api.example.com/items HTTP/1.1
	Content-Type: application/json
	Authorization: Bearer ${TOKEN}

	{
	  "name": "x"
	}
	HTTP/1.1 201 Created
	Content-Length: 28
	Content-Type: application/json
	Date: Mon, 01 Jan 2035 00:00:00 GMT

	{
	  "id": 7,
	  "name": "x"
	}
	-- items/list --
	GET https://api.example.com/items HTTP/1.1
	Authorization: Bearer ${TOKEN}

	HTTP/1.1 200 OK
	Content-Length: 32
	Content-Type: application/json
	Date: Mon, 01 Jan 2035 00:00:00 GMT

	[
	  {
	    "id": 7,
	    "name": "x"
	  }
	]

A response begins at the first line that begins one: HTTP/, a version, and a
status code. Everything above it is what you wrote, in the form rr stores it.
Running formats the request first: a known method is upper-cased, a standard
header name is respelled and written ahead of the custom ones, which are left
alone, and a JSON body is indented when Content-Type says it is JSON. Rr fmt
does that formatting on its own, over the whole file. A failed exchange is
left as it was, and so is every one below it; what answered above it is
stored, so the diff says how far the run got.

Rr run -match takes a regular expression and sends only the exchanges whose
name it matches, still in the order the file has them. It is unanchored, so
write ^ to mean the beginning.

	rr run -match '^items/' api.rr

A pattern that matches nothing is an error rather than a quiet success: a run
that did nothing and a run that changed nothing leave the same clean diff.

Rr gen writes the requests as commands for another program and sends nothing.
The only form so far is curl, which is what it writes when none is named. It
expands ${NAME} and $NAME the way rr run does, so what it writes is the
request rr run would send, values and all, and is not itself safe to commit.

	$ TOKEN=$(pass show api/token) rr gen -match '^items/create' items.rr
	# items/create
	curl -X POST https://api.example.com/items \
	  -H 'Authorization: Bearer 9f3c1e...' \
	  -H 'Content-Type: application/json' \
	  --data-raw '{
	  "name": "x"
	}'

Piping that to a shell sends the request curl's way, which is the point of
having it: the file goes to someone who has curl and has never heard of rr.
The name above each command is a shell comment, so it survives the pipe and
says which request is which.

The response is stored as it came off the wire. Redirects are not followed, so
it is the answer to this request rather than the one at the end of a chain.

A request and its last answer being one text file is the point of the tool.
The exchanges that have to run in order belong in one file, in that order; a
directory of files is still a collection, the environment is the environment,
sharing is git push, and git diff after a run reports what an API changed.

	for f in api/*.rr; do rr run "$f" >/dev/null; done
	git diff --exit-code api/

Rr has no cookie jar, no OAuth flow, and no assertions. An exchange cannot
read what the one above it answered: what passes between them passes through
the environment. A multipart body is sent as written, boundaries and all, but
there is no helper to compose one, and no way to attach a file that is not
text. A body is sent without the newline that ends it, and there is no way to
send one that ends in a newline. A body holding a line that would open an
exchange, or one that begins like a response, is cut short there; the format
has no escape, that being what keeps it plain text. A request over TLS may
negotiate HTTP/2 and be stored as such. Each request gets thirty seconds, and
^C cancels one in flight, leaving stored whatever answered before it.
