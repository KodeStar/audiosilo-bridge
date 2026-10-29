# cfi-golden

Writes `internal/epubcfi/testdata/golden.json`: CFIs made and resolved by foliate's own
`epubcfi.js` on jsdom over the fixture EPUBs, which `internal/epubcfi`'s golden tests
hold the Go port to in both directions (Bridge Phase 3c, decision 27). It also rebuilds
`testdata/synthetic.epub` from `synthetic/`.

Run it only when the player's foliate-js pin moves (or a fixture changes); CI runs Go only.

```sh
export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH"
cp ~/dev/audiosilo/audiosilo-frontend/reader/vendor/foliate-js/epubcfi.js .
# update PIN.txt from reader/vendor/FOLIATE_PIN.txt (the commit and epubcfi.js's sha256)
npm ci && node gen.mjs
cd ../.. && go test ./internal/epubcfi/
```

`TestGoldenPin` checks the copied `epubcfi.js` against `PIN.txt` and that `golden.json`
was written with it.

Two jsdom (26.1) quirks the generator works around, both CDATA: a Range boundary inside a
CDATASection is refused (those ranges are left out), and `Range.toString` leaves CDATA
out where a browser keeps it (those ranges keep their CFI check, `cdata: true` skips the
text check).

`alice.epub` is Project Gutenberg #11 (EPUB3, the spike's corpus copy) with its cover
image removed.
