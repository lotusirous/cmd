RR(1)                                                                    RR(1)

NAME
     rr - keep HTTP requests and their responses in one plain text file

SYNOPSIS
     rr run [-match regexp] [-omit regexp] [-timeout duration] file
     rr fmt file
     rr gen [-match regexp] [-form form] file

DESCRIPTION
     Rr sends the HTTP requests stored in a plain text file and writes
     each response back into the same file, under the request that made
     it.  The name is those two halves: request and response.

     The file is text and nothing more: it holds the protocol itself, laid
     out after txtar.  A marker line opens each exchange and names it:

          -- items/create --

     Text above the first marker is a comment rr does not read, and no two
     exchanges may answer to one name.  Rr sends them in the order the file
     has them and stops at the first failure.

     An exchange is wire-format HTTP with an absolute request URI, followed
     by the response last stored for it, which begins at the first line
     that begins one: HTTP/, a version, and a status code.  Everything
     after the blank line is the body; rr frames it, so Content-Length need
     not be written by hand.  ${NAME} and $NAME are read from the
     environment when the request is sent, and the file keeps the
     unexpanded text, so it is safe to commit.

     rr run sends the requests and stores the responses as they came off
     the wire, redirects not followed.  It rewrites the answer half and
     nothing else.  A failed exchange is left as it was, and so is every
     one below it, so the diff says how far the run got.

     rr fmt is the one command that rewrites a request, and it sends
     nothing: a method and the standard header names are made canonical,
     framing headers are dropped, and a body is indented when Content-Type
     declares it JSON.  Formatting a formatted file changes nothing.

     rr gen writes each request as a command for another program, on
     standard output, and leaves the file alone.  It expands ${NAME}, so
     what it writes is what rr run would send, values and all, and is not
     itself safe to commit.

OPTIONS
     -match regexp
            Send only the exchanges whose name regexp matches, in the
            order the file has them.  The pattern is unanchored.
            Matching no exchange is an error.

     -omit regexp
            Store no response header whose canonical name regexp
            matches: -omit '^(Date|X-Amz-)' keeps a moving date and a
            fresh request id out of the diff.  The pattern is unanchored
            and names response headers only.  Content-Length is always
            kept, being rr's own framing.

     -timeout duration
            Give each exchange a deadline, in Go's duration syntax: 1s,
            500ms, 2m30s.  It covers one whole exchange, from the
            connection to the last byte.  Without it a request waits
            forever.

     -form form
            Write rr gen's commands in this form.  Curl is the default
            and so far the only one.

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

     Send only some of them, and wait no longer than a moment for each:

          $ rr run -match '^items/' -timeout 5s api.rr

     Keep the chatty half of an answer out of the file, so the diff says
     what changed rather than when it was fetched:

          $ rr run -omit '^(Date|X-Amz-|X-Request-Id$)' api.rr

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
     in an exchange is named at once, rather than one to a run.  A pattern
     that does not compile is reported under the flag that held it, before
     the file is read.

BUGS
     Rr has no cookie jar, no OAuth flow, and no assertions.  An exchange
     cannot read what the one above it answered: what passes between them
     passes through the environment.

     A Host header is ignored, the absolute request URI carrying the host.

     A multipart body is sent as written, boundaries and all, but there is
     no helper to compose one, and no way to attach a file that is not
     text.  A body is sent without the newline that ends it, and there is
     no way to send one that ends in a newline; a blank line the file holds
     is part of the body, and rr fmt is what trims one.

     A body holding a line that would open an exchange, or one that begins
     like a response, is cut short there.  The format has no escape, that
     being what keeps it plain text.

     A request over TLS may negotiate HTTP/2 and be stored as such.  A
     request waits forever unless -timeout says otherwise, and ^C cancels
     one in flight, leaving stored whatever answered before it.
