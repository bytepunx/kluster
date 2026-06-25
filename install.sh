#!/usr/bin/env bash
set -euo pipefail

REPO="bytepunx/kluster"
BINARY="kluster"
INSTALL_DIR="/usr/local/bin"

# ── colours ──────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  GREEN="\033[32m"; YELLOW="\033[33m"; RED="\033[31m"; RESET="\033[0m"
else
  GREEN=""; YELLOW=""; RED=""; RESET=""
fi

info()    { printf "  ${GREEN}✓${RESET} %s\n" "$*"; }
warn()    { printf "  ${YELLOW}!${RESET} %s\n" "$*"; }
fatal()   { printf "  ${RED}✗${RESET} %s\n" "$*" >&2; exit 1; }
step()    { printf "  %s\n" "$*"; }

# ── platform detection ────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux|darwin) ;;
  *) fatal "unsupported OS: $OS" ;;
esac

RAW_ARCH=$(uname -m)
case "$RAW_ARCH" in
  x86_64)        ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) fatal "unsupported architecture: $RAW_ARCH" ;;
esac

PLATFORM="${OS}-${ARCH}"
step "Platform: ${PLATFORM}"

# ── latest version ────────────────────────────────────────────────────────────
step "Fetching latest release..."
API_RESPONSE=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null || true)
VERSION=$(printf '%s' "$API_RESPONSE" \
  | grep '"tag_name"' \
  | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' \
  || true)

if [ -z "$VERSION" ]; then
  fatal "could not determine latest release — check https://github.com/${REPO}/releases"
fi
step "Latest version: ${VERSION}"

# ── download ──────────────────────────────────────────────────────────────────
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY}-${PLATFORM}"
CHECKSUM_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

step "Downloading ${BINARY}-${PLATFORM}..."
if ! curl -fsSL "$DOWNLOAD_URL" -o "$TMP"; then
  fatal "download failed — no binary available for ${PLATFORM} in release ${VERSION}"
fi
chmod +x "$TMP"

# ── checksum verification ─────────────────────────────────────────────────────
if command -v sha256sum >/dev/null 2>&1; then
  SHA_CMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_CMD="shasum -a 256"
else
  SHA_CMD=""
fi

if [ -n "$SHA_CMD" ]; then
  CHECKSUMS=$(curl -fsSL "$CHECKSUM_URL" 2>/dev/null || true)
  EXPECTED=$(printf '%s' "$CHECKSUMS" \
    | grep "${BINARY}-${PLATFORM}" \
    | awk '{print $1}' \
    || true)
  if [ -n "$EXPECTED" ]; then
    ACTUAL=$(eval "$SHA_CMD" "$TMP" | awk '{print $1}')
    if [ "$ACTUAL" != "$EXPECTED" ]; then
      fatal "checksum mismatch — download may be corrupted"
    fi
    info "Checksum verified"
  fi
fi

# ── install ───────────────────────────────────────────────────────────────────
DEST="${INSTALL_DIR}/${BINARY}"
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP" "$DEST"
else
  step "Installing to ${DEST} (sudo required)..."
  sudo mv "$TMP" "$DEST"
fi
info "Installed ${DEST}"

# ── verify PATH ───────────────────────────────────────────────────────────────
FOUND=$(command -v "$BINARY" 2>/dev/null || true)

if [ "$FOUND" = "$DEST" ]; then
  info "kluster ${VERSION} is ready — run 'kluster setup' to verify prerequisites"
elif [ -n "$FOUND" ]; then
  warn "'which kluster' points to ${FOUND}, not ${DEST}"
  warn "Another version earlier in your PATH takes precedence."
  warn "Check the ordering of directories in your PATH, or run:"
  warn "  ${DEST} --version"
else
  warn "${INSTALL_DIR} is not in your PATH"
  warn "Add the following line to your shell profile and restart your shell:"
  case "${SHELL:-}" in
    */zsh)  warn "  echo 'export PATH=\"/usr/local/bin:\$PATH\"' >> ~/.zshrc" ;;
    */fish) warn "  fish_add_path /usr/local/bin" ;;
    *)      warn "  echo 'export PATH=\"/usr/local/bin:\$PATH\"' >> ~/.bashrc" ;;
  esac
fi
