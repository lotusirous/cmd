/*
rr sends the HTTP requests stored in a plain-text file and writes
each response back into the same file, under the request that made
it. The name is those two halves: request and response.

Everything rr knows is in that one file, and there is no format to
learn but HTTP itself. No database, no project file, no schema:
the file holds the protocol as it goes on the wire, each request
as it is sent and each response as it came back. Its layout is
txtar's, the archive format the Go tools keep their test cases in,
and so are its goals:

  - be trivial enough to write and edit by hand.
  - be HTTP and nothing else, but for the line that names an exchange.
  - diff nicely in git history and code reviews.

Rr is not a scripting language. It asserts nothing about a
response, carries no state from one exchange to the next, and
stores no body that is not text.

Usage:

	rr run [-match regexp] [-omit regexp] [-timeout duration] file
	rr fmt file
	rr gen [-match regexp] [-form form] file

'rr run' sends the requests and stores the responses. 'rr fmt' formats
the file, sends nothing, and leaves any stored response alone. 'rr
gen' writes the requests in another form and sends nothing either.
Curl is the form it writes when none is named, and so far the only
one there is.

# The file

A file is a txtar archive of exchanges. A marker line opens each
one and names it:

	-- items/create --
	POST https://api.example.com/items HTTP/1.1
	Content-Type: application/json

	{
	  "name": "x"
	}
	HTTP/1.1 201 Created
	Content-Type: application/json
	Content-Length: 28

	{
	  "id": 7,
	  "name": "x"
	}

Text before the first marker line is a comment. Rr does not read
it. A name is the writer's own, and no two exchanges in a file may
answer to one: -match selects on the name, and an error quotes it.

That is the whole of the format. It is text, so an editor, grep
and diff are all the tools it takes, and a file of exchanges
belongs in the repository beside the code that calls them.

An exchange is a wire-format HTTP request with an absolute-form
request URI, followed by the response last stored for it. The
response begins at the first line that begins one: HTTP/, a
version, and a status code.

Everything after the blank line is the body. Rr sends it without
the newline that ends it; a blank line the file holds is part of
it. Rr frames the request itself, so Content-Length need not be
written by hand, nor kept up to date when the body changes. One
that has gone stale is ignored on the wire and kept on disk until
rr fmt drops it. A header block that stops at the last header is
ended on the way out, and the file is left as it was.

${NAME} and $NAME are expanded from the environment before a
request is sent. The file on disk keeps the unexpanded text, so it
stays safe to commit.

# Running

Rr sends the exchanges in the order the file has them and stops at
the first failure, an exchange being free to rely on the one above
it. A run rewrites the response half and nothing else. The request
goes as the file writes it, and rr fmt is what puts one in
canonical form.

Nothing gives up on its own. A request waits as long as the server
takes to answer it, and ^C is how a run is called off. -timeout
gives each exchange a deadline instead, written the way Go writes
a duration: 1s, 500ms, 2m30s. It covers the whole of one exchange,
from the connection to the last byte of the body, rather than the
run.

A response is stored as it came back, less the headers -omit
names. The pattern is unanchored, as -match is, and is matched
against the canonical name of each header. For example,

	-omit '^(Date|X-Amz-)'

keeps out a date that moves every run and a request id that is new
every time, so what a diff is left with is what changed. It says
what the file keeps and no more. The request is the writer's own
text and keeps every line of it. Content-Length outlives any
pattern: it is rr's framing of the body it stores, not a header
the server sent.

# Formatting

Formatting is all that rr fmt does. A known method is upper-cased.
CRLF and folded lines are undone. Each header colon is followed by
one space. The standard header names are respelled and written
ahead of the custom ones, whose casing is their author's.
Content-Length and Transfer-Encoding are dropped, rr framing the
request. Header lines sharing a name keep their order.

A body is indented only when Content-Type declares it JSON:
application/json, or a type ending in +json. A body that merely
parses as JSON is left alone, nothing having said it was JSON, and
so is one that says it is JSON and does not parse.

Rr fmt rewrites the text and not the request. ${NAME} is left
unexpanded, and formatting a formatted file changes nothing. It
formats every request in the file, there being no -match, and
touches nothing else: the comment and the stored responses stay as
they were.

# Generating

Rr gen writes each matching request as a command for another
program, on standard output, and leaves the file alone. The form
is -form, and curl is the default and so far the only one. Each
command is written under the name of its exchange, as a comment,
so a pipe to a shell says which request is which. The request is
expanded first, so what gen writes is what run would send, values
and all, and is not itself safe to commit.

The curl form says what the request says and no more: no -s, no
-i, no -L. Header names come out canonical and sorted, X-API-KEY
as X-Api-Key, an [http.Header] having kept neither the spelling
nor the order the file had. A body goes in --data-raw, which keeps
the newlines an indented JSON body has; one that is not text is
read from standard input instead.
*/
package main
