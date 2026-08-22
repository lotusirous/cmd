RR(1)                                                                    RR(1)

NAME
     rr - send the HTTP requests stored in a file, store the responses in it

SYNOPSIS
     rr run [ -match regexp ] file
     rr fmt file
     rr gen [ -match regexp ] [ -form form ] file

DESCRIPTION
     Rr sends the HTTP requests stored in a file and writes each response
     back into the same file, under the request that made it.  The name is
     those two halves: request and response.

     The file is plain text and holds the protocol itself: each request as
     it goes on the wire, each response as it came back.  Its layout is
     txtar's, the archive format the Go tools keep their test cases in, and
     so are its goals:

          - be trivial enough to write and edit by hand.
          - be HTTP and nothing else, but for the line that names an
            exchange.
          - diff nicely in git history and code reviews.

     Non-goals include being a scripting language, asserting anything about
     a response, carrying state from one exchange to the next, and storing
     a body that is not text.

     A file holds a collection of exchanges, each opened by a txtar marker
     line naming it:

          -- items/create --

     Text above the first marker is a comment rr does not read.  A name is
     the writer's own, and it is what -match selects on and what an error
     quotes, so no two exchanges in a file may answer to one.  Rr sends
     them in the order the file has them and stops at the first failure, an
     exchange being free to rely on the one above it.

     A request is wire-format HTTP with an absolute request URI.
     Everything after the blank line is the body; rr measures it, and
     Content-Length need be neither written by hand nor kept up to date.
     ${NAME} and $NAME are read from the environment when the request is
     sent, in the body as well as the headers, and the file keeps the
     unexpanded text, so it is safe to commit.

     A response begins at the first line that begins one: HTTP/, a version,
     and a status code.  Everything above it is what you wrote, in the form
     rr stores it.  It is stored as it came off the wire, and redirects are
     not followed, so it is the answer to this request rather than the one
     at the end of a chain.

     Rr run sends the requests and stores the responses, formatting each
     request first, so a file written by hand ends up stored the way rr
     would have written it.  A failed exchange is left as it was, and so is
     every one below it; what answered above it is stored, so the diff says
     how far the run got.  -match takes a regular expression and sends only
     the exchanges whose name it matches, still in the order the file has
     them.  It is unanchored, so write ^ to mean the beginning.  A pattern
     that matches nothing is an error rather than a quiet success: a run
     that did nothing and a run that changed nothing leave the same clean
     diff.

     Rr fmt does that formatting on its own, over the whole file, and sends
     nothing.  A known method is upper-cased, CRLF and folded lines are
     undone, a header colon is followed by one space, the standard header
     names are respelled and written ahead of the custom ones, whose casing
     is their author's, and Content-Length and Transfer-Encoding are
     dropped, rr framing the request.  Header lines that share a name keep
     their order.  A body is indented only when Content-Type declares it
     JSON: application/json, or a type ending in +json.  A body that merely
     parses as JSON is left alone, nothing having said it was JSON, and so
     is one that says it is JSON and does not parse.  Rr fmt formats every
     request in the file, there being no -match, and touches nothing else:
     the comment, the stored responses, and ${NAME} are left as they were.
     Formatting a formatted file changes nothing.

     Rr gen writes each request as a command for another program, on
     standard output, and leaves the file alone.  The form is -form, and
     curl is the default and so far the only one; a form rr does not know
     is reported before the file is read.  -match narrows it as it does rr
     run.  It formats and expands ${NAME} the way rr run does, so what it
     writes is the request rr run would send, values and all, and is not
     itself safe to commit.  The command says what the request says and no
     more: no -s, no -i, no -L.  Header names come out canonical and
     sorted, X-API-KEY as X-Api-Key, the header map having kept neither the
     spelling nor the order the file had, and a header written with no
     value takes the semicolon curl reads as send it empty.  The body goes
     in --data-raw, which keeps the newlines an indented JSON body has, and
     a body that is not text is read from standard input instead, the
     command saying so rather than pretending it runs as it stands.

EXAMPLES
     Send the exchanges in a file, and read back what the file now holds:

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
          Content-Length: 40
          Content-Type: application/json
          Date: Mon, 01 Jan 2035 00:00:00 GMT

          [
            {
              "id": 7,
              "name": "x"
            }
          ]

     Send only some of them:

          $ rr run -match '^items/' api.rr

     Put a file written by hand into the form rr stores it in:

          $ cat items.rr
          --  items/create  --
          post https://api.example.com/items HTTP/1.1
          x-request-id: 7
          content-type:application/json
          Content-Length: 9
          accept: */*

          {"name":"x"}
          $ rr fmt items.rr
          $ cat items.rr
          -- items/create --
          POST https://api.example.com/items HTTP/1.1
          Content-Type: application/json
          Accept: */*
          x-request-id: 7

          {
            "name": "x"
          }

     Hand a request to someone who has curl and has never heard of rr.  The
     name above each command is a shell comment, so it survives a pipe to a
     shell and says which request is which:

          $ TOKEN=$(pass show api/token) rr gen -match '^items/create' items.rr
          # items/create
          curl -X POST https://api.example.com/items \
            -H 'Authorization: Bearer 9f3c1e...' \
            -H 'Content-Type: application/json' \
            --data-raw '{
            "name": "x"
          }'

     Run a directory of them and let git say what an API changed.  A
     request and its last answer being one text file is the point of the
     tool: the exchanges that have to run in order belong in one file, in
     that order; a directory of files is still a collection, the
     environment is the environment, and sharing is git push.

          $ for f in api/*.rr; do rr run "$f" >/dev/null; done
          $ git diff --exit-code api/

SOURCE
     https://github.com/lotusirous/cmd/tree/main/rr

          go install github.com/lotusirous/cmd/rr@latest

SEE ALSO
     curl(1), git-diff(1)

     golang.org/x/tools/txtar, the archive format this one is laid out
     after, and where the marker line comes from.

DIAGNOSTICS
     Rr exits 0 when every selected exchange was sent and stored, 2 when it
     was called wrongly, and 1 otherwise.  An error names the file and,
     where there is one, the exchange: parse for a file that is not one,
     match for a pattern that selects nothing, expand for a variable the
     environment does not hold, send for a request that did not go, and
     read for a response that did not arrive whole.  Every unset variable
     in an exchange is named at once, rather than one to a run.

BUGS
     Rr has no cookie jar, no OAuth flow, and no assertions.  An exchange
     cannot read what the one above it answered: what passes between them
     passes through the environment.

     A Host header is ignored, the absolute request URI carrying the host.

     A multipart body is sent as written, boundaries and all, but there is
     no helper to compose one, and no way to attach a file that is not
     text.  A body is sent without the newline that ends it, and there is
     no way to send one that ends in a newline.

     A body holding a line that would open an exchange, or one that begins
     like a response, is cut short there.  The format has no escape, that
     being what keeps it plain text.

     A request over TLS may negotiate HTTP/2 and be stored as such.  Each
     request gets thirty seconds, and ^C cancels one in flight, leaving
     stored whatever answered before it.
