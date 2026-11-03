#!/usr/bin/env bash
# Builds SpaceMan release archives, SBOMs and SHA256SUMS (site/operators/install.md, ADR-0034).
#
# Usage: scripts/release-build.sh [-o OUTDIR] [PLATFORM...]
#   PLATFORM is GOOS/GOARCH; default: linux/amd64 linux/arm64 (the supported production targets, SM-PKG-003).
#   darwin/arm64 and darwin/amd64 build only when named, for development use; their archives are labelled
#   "unsupported" in the file name and carry UNSUPPORTED-PLATFORM.txt.
#
# Environment (all optional; derived from git when unset):
#   VERSION            version string embedded in the binaries, e.g. v1.2.3 (default: git describe, else "dev")
#   COMMIT             full commit hash (default: git rev-parse HEAD)
#   SOURCE_DATE_EPOCH  build timestamp in Unix seconds (default: commit time); also used for archive mtimes
#   BUILDVCS           true|false, passed to go build -buildvcs (default: true inside a git work tree)
#   SBOM               auto (default: SPDX SBOMs when syft is on PATH, otherwise skipped with a notice) | require | skip
#   SYFT               syft command (default: syft)
#   ALLOW_NO_UI=1      build spaceman without the embedded UI when web/dist is missing (never for releases)
#   GO                 go command (default: go)
#
# Each archive spaceman_<version>_<os>_<arch>.tar.gz holds spaceman (UI embedded), spaceman-agent, the systemd units,
# example configs, LICENSE, NOTICE, THIRD_PARTY.md and CHANGELOG.md. It contains no herdr-core and no harness CLIs.
# Binaries are static (CGO_ENABLED=0), built with -trimpath and an empty build ID, so equal inputs give equal bytes.
# Archives are reproducible when GNU tar is available (CI); BSD tar output is deterministic in content but not in bytes.
set -euo pipefail

usage() { sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; }

out=dist
while getopts ":o:h" opt; do
  case $opt in
    o) out=$OPTARG ;;
    h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
platforms=("$@")
if [[ ${#platforms[@]} -eq 0 ]]; then
  platforms=(linux/amd64 linux/arm64)
fi
for platform in "${platforms[@]}"; do
  case $platform in
    linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
    *) echo "release-build: unsupported platform '$platform' (linux/amd64, linux/arm64; darwin/* for development only)" >&2; exit 2 ;;
  esac
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

go_cmd=${GO:-go}
in_git=false
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  in_git=true
fi

version=${VERSION:-}
if [[ -z $version ]]; then
  if $in_git; then version=$(git describe --tags --always --dirty 2>/dev/null || echo dev); else version=dev; fi
fi
if [[ ! $version =~ ^[0-9A-Za-z][0-9A-Za-z.+_-]*$ ]]; then
  echo "release-build: invalid VERSION '$version'" >&2
  exit 2
fi
commit=${COMMIT:-}
if [[ -z $commit ]] && $in_git; then commit=$(git rev-parse HEAD 2>/dev/null || true); fi
if [[ -n $commit && ! $commit =~ ^[0-9A-Za-z]+$ ]]; then
  echo "release-build: invalid COMMIT '$commit'" >&2
  exit 2
fi
epoch=${SOURCE_DATE_EPOCH:-}
if [[ -z $epoch ]] && $in_git; then epoch=$(git log -1 --format=%ct 2>/dev/null || true); fi
epoch=${epoch:-$(date +%s)}
if [[ ! $epoch =~ ^[0-9]+$ ]]; then
  echo "release-build: invalid SOURCE_DATE_EPOCH '$epoch'" >&2
  exit 2
fi
buildvcs=${BUILDVCS:-$in_git}
sbom_mode=${SBOM:-auto}
syft_cmd=${SYFT:-syft}
case $sbom_mode in
  auto | require | skip) ;;
  *) echo "release-build: SBOM must be auto, require or skip, got '$sbom_mode'" >&2; exit 2 ;;
esac
make_sbom=false
if [[ $sbom_mode != skip ]]; then
  if command -v "$syft_cmd" >/dev/null 2>&1; then
    make_sbom=true
  elif [[ $sbom_mode == require ]]; then
    echo "release-build: SBOM=require but '$syft_cmd' is not installed (https://github.com/anchore/syft)" >&2
    exit 1
  else
    echo "release-build: NOTICE: syft is not installed; SBOMs are skipped (set SBOM=require to fail instead)" >&2
  fi
fi

# RFC 3339 UTC from epoch, on GNU and BSD date.
build_date=$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$epoch" +%Y-%m-%dT%H:%M:%SZ)
touch_stamp=$(date -u -d "@$epoch" +%Y%m%d%H%M.%S 2>/dev/null || date -u -r "$epoch" +%Y%m%d%H%M.%S)

pkg=github.com/aurimasniekis/go-spaceman/internal/buildinfo
ldflags="-s -w -buildid= -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.Date=$build_date"
file_version=${version#v}
file_version=${file_version//+/_}

gnu_tar=""
for t in gtar tar; do
  if command -v "$t" >/dev/null 2>&1 && "$t" --version 2>/dev/null | grep -q 'GNU tar'; then
    gnu_tar=$t
    break
  fi
done

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# Release binaries embed the built UI (web/dist) via the embedui build tag.
tags=embedui
if [[ ! -f web/dist/index.html ]]; then
  if [[ "${ALLOW_NO_UI:-}" != "1" ]]; then
    echo "release-build: web/dist/index.html missing: run 'make web-build' first (or ALLOW_NO_UI=1 for a UI-less build)" >&2
    exit 1
  fi
  echo "release-build: WARNING: building spaceman WITHOUT the embedded UI (ALLOW_NO_UI=1)" >&2
  tags=
fi

mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

outputs=()
for platform in "${platforms[@]}"; do
  goos=${platform%/*}
  goarch=${platform#*/}
  name="spaceman_${file_version}_${goos}_${goarch}"
  [[ $goos == linux ]] || name="${name}_unsupported"
  stage="$work/$name"
  mkdir -p "$stage/systemd" "$stage/examples"
  echo "release-build: $platform -> $name.tar.gz" >&2
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch GOFLAGS=-mod=readonly \
    "$go_cmd" build -tags "$tags" -trimpath -buildvcs="$buildvcs" -ldflags "$ldflags" -o "$stage/spaceman" ./cmd/spaceman
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch GOFLAGS=-mod=readonly \
    "$go_cmd" build -trimpath -buildvcs="$buildvcs" -ldflags "$ldflags" -o "$stage/spaceman-agent" ./cmd/spaceman-agent
  cp deploy/systemd/spaceman.service deploy/systemd/spaceman-agent.service "$stage/systemd/"
  cp deploy/examples/spaceman.example.yaml deploy/examples/agent.example.yaml "$stage/examples/"
  cp LICENSE NOTICE CHANGELOG.md docs/THIRD_PARTY.md "$stage/"
  if [[ $goos != linux ]]; then
    cat >"$stage/UNSUPPORTED-PLATFORM.txt" <<EOF
This $goos/$goarch build is for development and evaluation only. SpaceMan's production target is Linux
(SM-PKG-003); $goos is not tested as a production platform and the systemd units do not apply to it.
EOF
  fi
  chmod 0755 "$stage/spaceman" "$stage/spaceman-agent" "$stage/systemd" "$stage/examples"
  find "$stage" -type f ! -name spaceman ! -name spaceman-agent -exec chmod 0644 {} +
  find "$stage" -exec env TZ=UTC touch -h -t "$touch_stamp" {} +

  archive="$out/$name.tar.gz"
  if [[ -n $gnu_tar ]]; then
    (cd "$work" && "$gnu_tar" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
      --format=pax --pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime \
      -cf - "$name") | gzip -n -9 >"$archive"
  else
    (cd "$work" && LC_ALL=C find "$name" -print | LC_ALL=C sort |
      tar --uid 0 --gid 0 --uname '' --gname '' --no-recursion -cf - -T -) | gzip -n -9 >"$archive"
  fi
  outputs+=("$(basename "$archive")")

  if $make_sbom; then
    # Go module dependencies of both binaries, read from their embedded build information.
    sbom="$name.spdx.json"
    SYFT_CHECK_FOR_APP_UPDATE=false "$syft_cmd" scan "dir:$stage" --source-name "$name" --source-version "$version" \
      -q -o "spdx-json=$out/$sbom"
    outputs+=("$sbom")
  fi
done

if $make_sbom; then
  # Source tree, including the web UI's locked npm dependencies that are bundled into the embedded UI.
  sbom="spaceman_${file_version}_source.spdx.json"
  SYFT_CHECK_FOR_APP_UPDATE=false "$syft_cmd" scan "dir:$root" --source-name spaceman --source-version "$version" \
    --exclude './.git' --exclude './**/node_modules' --exclude './bin' --exclude './dist' \
    --exclude "./${out#"$root"/}" --exclude './site/**' --exclude './test/**' --exclude './tools/**' \
    -q -o "spdx-json=$out/$sbom"
  outputs+=("$sbom")
fi

(cd "$out" && sha256 "${outputs[@]}" >SHA256SUMS)
echo "release-build: wrote $out/SHA256SUMS" >&2
cat "$out/SHA256SUMS"
