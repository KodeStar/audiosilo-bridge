#!/bin/sh
# fetch-whisper <tag> <asset>: download a pinned whisper-cli release asset of
# audiosilo-sidecars, check its sha256 against that release's checksums.txt,
# and unpack it into /w/out (the Dockerfile's whisper stages).
set -eu
base="https://github.com/KodeStar/audiosilo-sidecars/releases/download/$1"
mkdir -p /w/out
cd /w
curl -fsSLo checksums.txt "$base/checksums.txt"
curl -fsSLo "$2" "$base/$2"
grep " $2\$" checksums.txt | sha256sum -c -
tar -xzf "$2" -C out
rm "$2"
chmod 0755 out/whisper-cli
