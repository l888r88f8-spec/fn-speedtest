#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
PACKAGE_DIR="$ROOT_DIR/package"
DIST_DIR="$ROOT_DIR/dist"
VERSION="1.10.13"
OUT_FPK="$DIST_DIR/fnos-speedtest-${VERSION}-x86_64.fpk"

mkdir -p "$PACKAGE_DIR/app/bin" "$PACKAGE_DIR/app/ui/images" "$PACKAGE_DIR/app/licenses" "$PACKAGE_DIR/wizard" "$DIST_DIR"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o "$PACKAGE_DIR/app/bin/fnos-speedtest" "$ROOT_DIR"

SPEEDTEST_GO_VERSION="1.8.3"
SPEEDTEST_GO_ARCHIVE="$DIST_DIR/speedtest-go_${SPEEDTEST_GO_VERSION}_Linux_x86_64.tar.gz"
SPEEDTEST_GO_URL="https://github.com/showwin/speedtest-go/releases/download/v${SPEEDTEST_GO_VERSION}/speedtest-go_${SPEEDTEST_GO_VERSION}_Linux_x86_64.tar.gz"
SPEEDTEST_GO_SHA256="c55caae22927cd719a2f8c0bef96ecf74b5fe8ca919ff9200d76328ae5de9477"
curl -fL --retry 3 --retry-delay 2 "$SPEEDTEST_GO_URL" -o "$SPEEDTEST_GO_ARCHIVE"
echo "$SPEEDTEST_GO_SHA256  $SPEEDTEST_GO_ARCHIVE" | sha256sum -c -
SPEEDTEST_GO_TMP="$(mktemp -d)"
tar -xzf "$SPEEDTEST_GO_ARCHIVE" -C "$SPEEDTEST_GO_TMP"
SPEEDTEST_GO_BIN="$(find "$SPEEDTEST_GO_TMP" -type f -name speedtest-go -print -quit)"
if [ -z "$SPEEDTEST_GO_BIN" ]; then
  echo "speedtest-go binary missing from official archive" >&2
  exit 1
fi
install -m 0755 "$SPEEDTEST_GO_BIN" "$PACKAGE_DIR/app/bin/speedtest-go"
rm -rf "$SPEEDTEST_GO_TMP"
ICON_TMP="$DIST_DIR/icon-512.png"
convert -size 512x512 xc:none \
  -fill '#0b1727' -stroke none -draw 'roundrectangle 24,24 488,488 116,116' \
  -fill none -stroke '#233b58' -strokewidth 32 -draw 'arc 111,202 401,492 180,360' \
  -stroke '#42d6ff' -strokewidth 32 -draw 'arc 111,202 401,492 180,315' \
  -stroke '#aeefff' -strokewidth 22 -draw 'line 256,108 256,155 line 119,165 153,199 line 393,165 359,199' \
  -stroke '#48e0aa' -strokewidth 28 -draw 'line 334,243 269,321' \
  -fill '#5688ff' -stroke none -draw 'circle 256,337 294,337' \
  -fill '#07111f' -draw 'circle 256,337 269,337' "$ICON_TMP"
convert "$ICON_TMP" -resize 64x64 "$PACKAGE_DIR/ICON.PNG"
convert "$ICON_TMP" -resize 256x256 "$PACKAGE_DIR/ICON_256.PNG"
cp "$PACKAGE_DIR/ICON.PNG" "$PACKAGE_DIR/app/ui/images/icon_64.png"
cp "$PACKAGE_DIR/ICON_256.PNG" "$PACKAGE_DIR/app/ui/images/icon_256.png"
chmod +x "$PACKAGE_DIR/cmd/"* "$PACKAGE_DIR/app/bin/fnos-speedtest" "$PACKAGE_DIR/app/bin/speedtest-go"

if command -v fnpack >/dev/null 2>&1; then
  (cd "$PACKAGE_DIR" && fnpack build)
  mv -f "$PACKAGE_DIR/fnos-speedtest.fpk" "$OUT_FPK"
else
  # fnpack 1.2.3 output is a gzip-compressed tar containing app.tgz and the
  # package metadata. This fallback mirrors that container layout so release
  # builds remain possible in isolated CI environments.
  STAGE="$(mktemp -d)"
  trap 'rm -rf "$STAGE"' EXIT
  tar -czf "$STAGE/app.tgz" -C "$PACKAGE_DIR/app" .
  MD5="$(md5sum "$STAGE/app.tgz" | awk '{print $1}')"
  cp "$PACKAGE_DIR/manifest" "$STAGE/manifest"
  if grep -q '^checksum=' "$STAGE/manifest"; then
    sed -i "s/^checksum=.*/checksum=$MD5/" "$STAGE/manifest"
  else
    printf '\nchecksum=%s\n' "$MD5" >> "$STAGE/manifest"
  fi
  cp "$PACKAGE_DIR/ICON.PNG" "$PACKAGE_DIR/ICON_256.PNG" "$STAGE/"
  cp -a "$PACKAGE_DIR/cmd" "$PACKAGE_DIR/config" "$STAGE/"
  [ ! -d "$PACKAGE_DIR/wizard" ] || cp -a "$PACKAGE_DIR/wizard" "$STAGE/"
  tar -czf "$OUT_FPK" -C "$STAGE" .
fi

sha256sum "$OUT_FPK" > "$OUT_FPK.sha256"
echo "Built: $OUT_FPK"
