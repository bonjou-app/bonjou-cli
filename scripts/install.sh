#!/usr/bin/env bash
set -euo pipefail

REPO="bonjou-app/bonjou-cli"
BREW_FORMULA="hamzaabdulwahab/bonjou/bonjou"
SCOOP_MANIFEST_URL="https://raw.githubusercontent.com/hamzaabdulwahab/scoop-bonjou/main/bonjou.json"

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

latest_version() {
  local api
  api="https://api.github.com/repos/${REPO}/releases/latest"
  curl -fsSL "$api" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1
}

install_binary_unix() {
  local version os arch asset url tmp_dir tmp_file target
  version="$(latest_version)"
  [[ -n "$version" ]] || fail "Could not resolve latest release version from GitHub API"

  os="$(uname -s)"
  arch="$(uname -m)"

  case "$os" in
    Darwin)
      asset="bonjou-macos"
      ;;
    Linux)
      case "$arch" in
        x86_64|amd64) asset="bonjou-linux-amd64" ;;
        aarch64|arm64) asset="bonjou-linux-arm64" ;;
        *) fail "Unsupported Linux architecture: $arch" ;;
      esac
      ;;
    *)
      fail "Unsupported OS for this installer: $os"
      ;;
  esac

  url="https://github.com/${REPO}/releases/download/v${version}/${asset}"
  tmp_dir="$(mktemp -d)"
  tmp_file="$tmp_dir/bonjou"
  target="/usr/local/bin/bonjou"

  log "Downloading Bonjou v${version} (${asset})..."
  curl -fL "$url" -o "$tmp_file"
  chmod +x "$tmp_file"

  if [[ -w "$(dirname "$target")" ]]; then
    install -m 0755 "$tmp_file" "$target"
  elif command_exists sudo; then
    sudo install -m 0755 "$tmp_file" "$target"
  else
    fail "Need write access to $(dirname "$target") or sudo installed"
  fi

  rm -rf "$tmp_dir"
  log "Installed to $target"
}

main() {
  local os
  os="$(uname -s)"

  if [[ "$os" == "Darwin" ]] && command_exists brew; then
    log "Homebrew found. Installing via formula ${BREW_FORMULA}..."
    brew install "$BREW_FORMULA"
    return
  fi

  if [[ "$os" =~ ^(MINGW|MSYS|CYGWIN|Windows_NT)$ ]]; then
    if command_exists scoop; then
      log "Scoop found. Installing via manifest URL..."
      scoop install "$SCOOP_MANIFEST_URL"
      return
    fi
    fail "Windows users should run scripts/install.ps1 from PowerShell"
  fi

  if command_exists curl; then
    install_binary_unix
    return
  fi

  fail "curl is required for non-Homebrew installation"
}

main "$@"
