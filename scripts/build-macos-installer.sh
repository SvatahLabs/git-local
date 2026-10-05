#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "macOS is required to build a .pkg installer" >&2
  exit 1
fi

version="${1:-0.1.4}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 [major.minor.patch]" >&2
  exit 2
fi

for command in arch lipo codesign pkgbuild pkgutil shasum xattr gzip cpio cmp; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required macOS tool is missing: $command" >&2
    exit 1
  fi
done

go_bin="${GITLOCAL_GO_BIN:-go}"
if ! command -v "$go_bin" >/dev/null 2>&1; then
  echo "Go is required to build the installer (set GITLOCAL_GO_BIN if needed)" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
output_dir="${GITLOCAL_DIST_DIR:-$repo_root/dist}"
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd -P)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/gitlocal-macos-pkg.XXXXXXXX")"
trap 'rm -rf "$work_dir"' EXIT

cd "$repo_root"
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$go_bin" build \
    -trimpath -buildvcs=false -ldflags="-buildid= -X gitlocal/internal/cli.Version=$version" \
    -o "$work_dir/git-local-$arch" ./cmd/git-local
done

payload="$work_dir/payload"
install_dir="$payload/usr/local/bin"
mkdir -p "$install_dir"
chmod 755 "$payload" "$payload/usr" "$payload/usr/local" "$install_dir"
lipo -create -output "$install_dir/git-local" \
  "$work_dir/git-local-arm64" "$work_dir/git-local-amd64"
chmod 755 "$install_dir/git-local"
# lipo changes the Mach-O image. Apply a local ad-hoc code signature after joining.
codesign --force --sign - "$install_dir/git-local"
# Do not package Finder/resource-fork metadata as AppleDouble ._ files.
xattr -cr "$payload"
codesign --verify --strict "$install_dir/git-local"
if [[ "$("$install_dir/git-local" --version)" != *"$version"* ]]; then
  echo "built git-local has the wrong version" >&2
  exit 1
fi

archs="$(lipo -archs "$install_dir/git-local")"
if [[ " $archs " != *" arm64 "* || " $archs " != *" x86_64 "* ]]; then
  echo "universal binary is missing an architecture: $archs" >&2
  exit 1
fi

# This checks Git's external-command discovery without installing or using Go.
PATH="$install_dir:/usr/bin:/bin" /usr/bin/git local > "$work_dir/help.txt"
if ! /usr/bin/grep -q 'Available Commands:' "$work_dir/help.txt"; then
  echo "built git-local failed its Git CLI smoke test" >&2
  exit 1
fi

package_name="GitLocal-${version}-macos-universal.pkg"
package_path="$output_dir/$package_name"
COPYFILE_DISABLE=1 pkgbuild --root "$payload" --install-location / \
  --identifier org.gitlocal.cli --version "$version" \
  --ownership recommended "$package_path"

pkgutil --payload-files "$package_path" > "$work_dir/payload-files.txt"
found_binary=0
while IFS= read -r entry; do
  case "$entry" in
    ./usr/local/bin/git-local) found_binary=$((found_binary + 1)) ;;
    .|./usr|./usr/local|./usr/local/bin|./._usr|./usr/._local|./usr/local/._bin|./usr/local/bin/._git-local) ;;
    *)
      echo "package payload has an unexpected path: $entry" >&2
      exit 1
      ;;
  esac
done < "$work_dir/payload-files.txt"
if [[ "$found_binary" != 1 ]]; then
  echo "package payload is missing git-local" >&2
  exit 1
fi
pkgutil --expand "$package_path" "$work_dir/expanded"
if [[ -e "$work_dir/expanded/Scripts" ]]; then
  echo "package unexpectedly contains installer scripts" >&2
  exit 1
fi
mkdir "$work_dir/extracted"
(cd "$work_dir/extracted" && gzip -dc "$work_dir/expanded/Payload" | cpio -idm --quiet)
extracted_binary="$work_dir/extracted/usr/local/bin/git-local"
cmp "$install_dir/git-local" "$extracted_binary"
codesign --verify --strict "$extracted_binary"
if [[ "$("$extracted_binary" --version)" != *"$version"* ]]; then
  echo "packaged git-local has the wrong version" >&2
  exit 1
fi
PATH="$work_dir/extracted/usr/local/bin:/usr/bin:/bin" /usr/bin/git local > "$work_dir/installed-help.txt"
if ! /usr/bin/grep -q 'Available Commands:' "$work_dir/installed-help.txt"; then
  echo "packaged git-local failed its Git CLI smoke test" >&2
  exit 1
fi

# On Apple Silicon with Rosetta (or native Intel), execute the x86_64 slice
# from the extracted package as well. The other slice remains structurally
# verified by lipo and codesign on hosts that cannot execute it.
if arch -x86_64 /usr/bin/true >/dev/null 2>&1; then
  if [[ "$(arch -x86_64 "$extracted_binary" --version)" != *"$version"* ]]; then
    echo "packaged x86_64 git-local has the wrong version" >&2
    exit 1
  fi
  PATH="$work_dir/extracted/usr/local/bin:/usr/bin:/bin" \
    arch -x86_64 /usr/bin/git local > "$work_dir/installed-help-x86_64.txt"
  if ! /usr/bin/grep -q 'Available Commands:' "$work_dir/installed-help-x86_64.txt"; then
    echo "packaged x86_64 git-local failed its Git CLI smoke test" >&2
    exit 1
  fi
fi

(cd "$output_dir" && shasum -a 256 "$package_name" > "$package_name.sha256")
echo "Built $package_path"
echo "SHA-256: $package_path.sha256"
echo "Unsigned installer; release distribution requires Developer ID signing and notarization."
