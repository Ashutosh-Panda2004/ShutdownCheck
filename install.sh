#!/bin/sh
# Install shutdowncheck.
#
#   curl -fsSL https://raw.githubusercontent.com/shutdowncheck/shutdowncheck/main/install.sh | sh
#
# Piping a script from the internet into a shell is a bad habit, so this one
# earns it: it downloads over TLS, verifies the SHA-256 against the signed
# checksums file, and refuses to install anything that does not match. Read it
# first — that is the point of it being short.
#
# POSIX sh on purpose: this has to run in a distroless-adjacent CI image with
# no bash.
#
# Environment:
#   SHUTDOWNCHECK_VERSION  version to install, e.g. v1.2.3 (default: latest)
#   SHUTDOWNCHECK_BIN_DIR  install directory (default: /usr/local/bin, or
#                          ~/.local/bin when that is not writable)

set -eu

REPO="shutdowncheck/shutdowncheck"
VERSION="${SHUTDOWNCHECK_VERSION:-latest}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

detect_os() {
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$os" in
        linux) printf 'linux' ;;
        darwin) printf 'darwin' ;;
        mingw*|msys*|cygwin*)
            die "use the Scoop package or a release zip on Windows: https://github.com/$REPO/releases"
            ;;
        *) die "unsupported operating system: $os" ;;
    esac
}

detect_arch() {
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) printf 'amd64' ;;
        aarch64|arm64) printf 'arm64' ;;
        *) die "unsupported architecture: $arch (binaries are published for amd64 and arm64)" ;;
    esac
}

resolve_version() {
    if [ "$VERSION" != "latest" ]; then
        printf '%s' "$VERSION"
        return
    fi

    # Follows the redirect from /releases/latest rather than parsing the API,
    # which keeps this working without a token and without jq.
    resolved=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')

    [ -n "$resolved" ] || die "could not determine the latest version; set SHUTDOWNCHECK_VERSION"
    printf '%s' "$resolved"
}

choose_bin_dir() {
    if [ -n "${SHUTDOWNCHECK_BIN_DIR:-}" ]; then
        printf '%s' "$SHUTDOWNCHECK_BIN_DIR"
        return
    fi
    if [ -w /usr/local/bin ] 2>/dev/null; then
        printf '/usr/local/bin'
        return
    fi
    printf '%s/.local/bin' "$HOME"
}

verify_checksum() {
    archive="$1"
    checksums="$2"

    if command -v sha256sum >/dev/null 2>&1; then
        # --ignore-missing so the file listing every platform's artefact does
        # not fail on the ones we did not download.
        sha256sum --check --ignore-missing "$checksums" >/dev/null 2>&1 && return 0
        return 1
    fi

    if command -v shasum >/dev/null 2>&1; then
        expected=$(grep " $(basename "$archive")\$" "$checksums" | awk '{print $1}')
        [ -n "$expected" ] || return 1
        actual=$(shasum -a 256 "$archive" | awk '{print $1}')
        [ "$expected" = "$actual" ]
        return $?
    fi

    die "neither sha256sum nor shasum is available, so the download cannot be verified"
}

main() {
    need curl
    need tar

    os=$(detect_os)
    arch=$(detect_arch)
    version=$(resolve_version)
    bin_dir=$(choose_bin_dir)

    archive="shutdowncheck_${version#v}_${os}_${arch}.tar.gz"
    base="https://github.com/$REPO/releases/download/$version"

    tmp=$(mktemp -d)
    # Never leave a half-extracted binary or a staged file behind on failure.
    trap 'rm -rf "$tmp"; rm -f "${staged:-}"' EXIT INT TERM

    log "downloading shutdowncheck $version ($os/$arch)"
    curl -fsSL -o "$tmp/$archive" "$base/$archive" \
        || die "download failed; check that $version has a $os/$arch build at https://github.com/$REPO/releases"
    curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" \
        || die "could not download checksums.txt, so the archive cannot be verified"

    log "verifying checksum"
    ( cd "$tmp" && verify_checksum "$archive" "checksums.txt" ) \
        || die "CHECKSUM MISMATCH for $archive - refusing to install. Do not use this download."

    tar -xzf "$tmp/$archive" -C "$tmp" shutdowncheck \
        || die "could not extract shutdowncheck from $archive"

    mkdir -p "$bin_dir"
    # Written under a temporary name and renamed, so a running binary is never
    # replaced underneath itself. cp rather than install(1), which is not
    # present on every minimal image.
    staged="$bin_dir/.shutdowncheck.$$"
    cp "$tmp/shutdowncheck" "$staged" 2>/dev/null \
        || die "cannot write to $bin_dir; set SHUTDOWNCHECK_BIN_DIR or re-run with sudo"
    chmod 0755 "$staged"
    mv "$staged" "$bin_dir/shutdowncheck"

    log "installed $bin_dir/shutdowncheck"

    case ":$PATH:" in
        *":$bin_dir:"*) ;;
        *) log ""; log "note: $bin_dir is not on your PATH" ;;
    esac

    log ""
    log "Try it:  shutdowncheck demo"
}

main "$@"
