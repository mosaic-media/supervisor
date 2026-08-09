# Vendored, not fetched

htmx 2.0.4 and its SSE extension, copied here and embedded in the Supervisor
binary. **They are never fetched at runtime** — the recovery UI draws when there
is no Shell and possibly no route to the internet, so a CDN reference would be
one more thing that has to work on the worst day this install has.
`TestTheEmbeddedRendererFetchesNothingButItsOwnOrigin` asserts it.

htmx is 0BSD (see `LICENSE.htmx`), which imposes no attribution requirement;
the licence is kept anyway because vendored code should carry its terms.

To update: replace the two files, run the container gate, and check the
byte-size test still passes. Nothing generates them.
