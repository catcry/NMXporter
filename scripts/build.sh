#!/bin/bash
# Builds the distributable package:
#   dist/eventlink-exporters-<version>-linux-amd64.tar.gz
# Usage (from anywhere, Go in PATH or GO=/path/to/go):
#   bash scripts/build.sh            # version from the VERSION file
#   bash scripts/build.sh 1.7.3      # explicit version
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
VERSION=${1:-$(cat "$ROOT/VERSION")}
GO=${GO:-go}
NAME=eventlink-exporters-$VERSION-linux-amd64
STAGE=$ROOT/dist/$NAME
rm -rf "$STAGE" && mkdir -p "$STAGE"/{bin,config,systemd,prometheus,grafana,tools/stress}
cd "$ROOT"

echo "build binaries $VERSION"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
LD="-s -w -X main.version=$VERSION"
for b in directory-exporter eventlink-trace-exporter eventlink-db-exporter; do
	"$GO" build -trimpath -ldflags="$LD" -o "$STAGE/bin/$b" ./cmd/$b
done
unset GOOS GOARCH

cp scripts/exporterctl.sh "$STAGE/bin/exporterctl.sh"
cp scripts/install.sh scripts/uninstall.sh docs/MANUAL.md "$STAGE/"
cp scripts/PACKAGE-README.txt "$STAGE/README.txt"
cp docs/METRICS.md docs/DASHBOARD.md "$STAGE/"
cp config/*.in "$STAGE/config/"
cp systemd/*.in "$STAGE/systemd/"
cp prometheus/alerts.yml prometheus/scrape.yml.in "$STAGE/prometheus/"
cp grafana/eventlink-overview.json "$STAGE/grafana/"
cp tools/stress/*.sh "$STAGE/tools/stress/"
echo "$VERSION" >"$STAGE/VERSION"
git rev-parse --short=12 HEAD >"$STAGE/REVISION" 2>/dev/null || true

# Unix line endings for every text file (the repository may be checked out on Windows).
find "$STAGE" -type f ! -path "*/bin/*-exporter" -exec sed -i 's/\r$//' {} +

(cd "$STAGE" && find . -type f ! -name SHA256SUMS | sort | xargs sha256sum >SHA256SUMS)

echo "tar"
cd "$ROOT/dist"
EXEC="bin/directory-exporter bin/eventlink-trace-exporter bin/eventlink-db-exporter bin/exporterctl.sh install.sh uninstall.sh tools/stress/stress.sh tools/stress/trace-burst.sh tools/stress/db-stress.sh"
excl=()
for f in $EXEC; do excl+=(--exclude="$NAME/$f"); done
tar --owner=0 --group=0 --numeric-owner --mode='u=rwX,go=rX' "${excl[@]}" -cf "$NAME.tar" "$NAME"
tar --owner=0 --group=0 --numeric-owner --mode='u=rwx,go=rx' -rf "$NAME.tar" $(for f in $EXEC; do echo "$NAME/$f"; done)
gzip -9f "$NAME.tar"
sha256sum "$NAME.tar.gz" >"$NAME.tar.gz.sha256"
ls -la "$NAME.tar.gz"
