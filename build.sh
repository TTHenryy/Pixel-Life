#!/bin/sh
# Builds app/PixelLife.exe and app/version.json.
# Usage: PAGE=<page version> EXE=<exe version> ./build.sh
set -e
cd "$(dirname "$0")"
PAGE=${PAGE:-1}; EXE=${EXE:-1}
cp app/index.html src/index.html
sed -i "s/embeddedPageVersion = [0-9]*/embeddedPageVersion = $PAGE/; s/exeVersion          = [0-9]*/exeVersion          = $EXE/" src/main.go
(cd src && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet . && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-H windowsgui -s -w" -o ../app/PixelLife.exe .)
SHA=$(sha256sum app/PixelLife.exe | cut -d' ' -f1)
printf '{"page":%s,"exe":%s,"exeSha256":"%s"}\n' "$PAGE" "$EXE" "$SHA" > app/version.json
cat app/version.json
