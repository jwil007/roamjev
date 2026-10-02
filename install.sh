#!/bin/bash
# roamjev installer: downloads the latest release, verifies it, asks for your
# TypeSafe (Jev) API key, and optionally sets up a systemd service.
#
#   curl -fsSL https://raw.githubusercontent.com/jwil007/roamjev/main/install.sh -o /tmp/install.sh && bash /tmp/install.sh
set -e

REPO="jwil007/roamjev"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="roamjev"
KEY_DIR="/etc/roamjev"
KEY_FILE="$KEY_DIR/api_key"
API_URL="https://api.typesafe.ai/v1/systemone"

# Prompts read from the terminal, so this also works when piped into bash.
TTY=/dev/tty
ask() { printf "%s" "$1" > "$TTY"; read -r REPLY < "$TTY"; }
ask_secret() { printf "%s" "$1" > "$TTY"; read -rs REPLY < "$TTY"; printf "\n" > "$TTY"; }

if [ "$(uname -s)" != "Linux" ]; then
  echo "roamjev runs on Linux only."; exit 1
fi
for cmd in curl tar sha256sum; do
  command -v "$cmd" >/dev/null || { echo "Missing required command: $cmd"; exit 1; }
done

# Detect arch
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)         GOARCH="amd64" ;;
  aarch64|arm64)  GOARCH="arm64" ;;
  armv7*|armhf)   GOARCH="arm" ;;
  armv6*)         GOARCH="arm" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest release tag from GitHub API
LATEST=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | grep '"tag_name"' \
  | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
if [ -z "$LATEST" ]; then
  echo "Couldn't find a roamjev release on GitHub."; exit 1
fi
echo "Latest version: $LATEST"

SKIP_BINARY=false
if command -v $BINARY_NAME &>/dev/null; then
  INSTALLED=$($BINARY_NAME -version 2>/dev/null || echo "unknown")
  echo "Installed version: $INSTALLED"
  # The binary reports 0.1.0; release tags are v0.1.0.
  if [ "v${INSTALLED#v}" = "$LATEST" ]; then
    echo "Binary already up to date."
    SKIP_BINARY=true
  fi
fi

if [ "$SKIP_BINARY" = false ]; then
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  ARCHIVE="roamjev_${GOARCH}.tar.gz"
  BASE="https://github.com/$REPO/releases/download/$LATEST"
  echo "Downloading $ARCHIVE..."
  curl -fsSL "$BASE/$ARCHIVE" -o "$TMP/$ARCHIVE"
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
  echo "Verifying checksum..."
  (cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | sha256sum -c --quiet -) \
    || { echo "Checksum mismatch. Aborting."; exit 1; }
  tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
  chmod +x "$TMP/roamjev"
  echo "Installing to $INSTALL_DIR/$BINARY_NAME..."
  sudo install -m 0755 "$TMP/roamjev" "$INSTALL_DIR/$BINARY_NAME"
  sudo install -d -m 0755 /usr/local/share/roamjev
  sudo install -m 0644 "$TMP/systemd/roamjev@.service" /usr/local/share/roamjev/roamjev@.service
  echo "roamjev installed."
fi

# API key
check_key() {
  local code
  # The header comes from a file descriptor so the key never appears in ps.
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$API_URL" \
    -H @<(printf 'Authorization: Bearer %s\n' "$1") -H "Content-Type: application/json" \
    -d '{"model":"jev-1.13.0","state":"roamjev install check","questions":{"ok":{"type":"noul","instructions":"Is this a test?"}}}')
  echo "$code"
}

echo ""
echo "roamjev needs a TypeSafe API key for the Jev model (https://console.typesafe.ai)."
KEEP_KEY=false
if sudo test -s "$KEY_FILE"; then
  ask "An API key is already saved in $KEY_FILE. Keep it? [Y/n]: "
  if [ "$REPLY" != "n" ] && [ "$REPLY" != "N" ]; then
    KEEP_KEY=true
  fi
fi
if [ "$KEEP_KEY" = false ]; then
  while true; do
    ask_secret "Paste your API key (input hidden, Enter to skip): "
    KEY=$(echo "$REPLY" | tr -d '[:space:]')
    if [ -z "$KEY" ]; then
      echo "Skipped. Add it later: sudo install -m 600 /dev/stdin $KEY_FILE <<< 'your-key'"
      break
    fi
    printf "Checking key... "
    CODE=$(check_key "$KEY")
    case "$CODE" in
      200)
        echo "OK"
        sudo install -d -m 0755 "$KEY_DIR"
        printf "%s" "$KEY" | sudo tee "$KEY_FILE" >/dev/null
        sudo chmod 600 "$KEY_FILE"
        echo "Saved to $KEY_FILE (readable by root only)."
        break ;;
      401) echo "rejected (invalid key). Try again." ;;
      000) echo "couldn't reach TypeSafe. Saving it unchecked."
           sudo install -d -m 0755 "$KEY_DIR"
           printf "%s" "$KEY" | sudo tee "$KEY_FILE" >/dev/null
           sudo chmod 600 "$KEY_FILE"
           break ;;
      *)   echo "unexpected response (HTTP $CODE). Saving it anyway."
           sudo install -d -m 0755 "$KEY_DIR"
           printf "%s" "$KEY" | sudo tee "$KEY_FILE" >/dev/null
           sudo chmod 600 "$KEY_FILE"
           break ;;
    esac
  done
fi
unset KEY REPLY

# Detect wireless interface
echo ""
echo "Detecting wireless interfaces..."
# shellcheck disable=SC2011
IFACES=$(ls /sys/class/net/ | xargs -I{} sh -c 'test -d /sys/class/net/{}/wireless && echo {}' 2>/dev/null || true)
SELECTED=""
if [ -z "$IFACES" ]; then
  echo "No wireless interfaces found. Pass one later with: sudo roamjev -iface <name>"
else
  echo "Available wireless interfaces:"
  i=1
  for iface in $IFACES; do
    echo "  $i) $iface"
    i=$((i+1))
  done
  ask "Select interface [1]: "
  SELECTION=${REPLY:-1}
  SELECTED=$(echo "$IFACES" | sed -n "${SELECTION}p")
  [ -z "$SELECTED" ] && echo "Invalid selection. Pass one later with: sudo roamjev -iface <name>"
fi

# Systemd service
SERVICE_INSTALLED=false
ask $'\nInstall roamjev as a systemd service? [y/N]: '
if [ "$REPLY" = "y" ] || [ "$REPLY" = "Y" ]; then
  UNIT=/usr/local/share/roamjev/roamjev@.service
  if [ ! -f "$UNIT" ]; then
    UNIT=$(mktemp)
    curl -fsSL "https://raw.githubusercontent.com/$REPO/$LATEST/systemd/roamjev@.service" -o "$UNIT"
  fi
  sudo install -m 0644 "$UNIT" /etc/systemd/system/roamjev@.service
  sudo systemctl daemon-reload
  SERVICE_INSTALLED=true
  echo "Installed /etc/systemd/system/roamjev@.service"
  if [ -n "$SELECTED" ]; then
    if systemctl is-active --quiet "roamctl@$SELECTED" 2>/dev/null; then
      echo "roamctl@$SELECTED is running. Only one of roamctl and roamjev can control roaming."
      ask "Stop and disable roamctl@$SELECTED? [y/N]: "
      if [ "$REPLY" = "y" ] || [ "$REPLY" = "Y" ]; then
        sudo systemctl disable --now "roamctl@$SELECTED"
      fi
    fi
    ask "Enable roamjev@$SELECTED at boot? [y/N]: "
    if [ "$REPLY" = "y" ] || [ "$REPLY" = "Y" ]; then
      sudo systemctl enable "roamjev@$SELECTED"
    fi
    ask "Start roamjev@$SELECTED now? [y/N]: "
    if [ "$REPLY" = "y" ] || [ "$REPLY" = "Y" ]; then
      sudo systemctl start "roamjev@$SELECTED"
      echo "roamjev@$SELECTED started."
    fi
  fi
fi

# Summary
IF=${SELECTED:-<iface>}
echo ""
echo "================================================"
echo " roamjev $LATEST installed"
echo "================================================"
echo ""
echo " Run in foreground:  sudo roamjev -iface $IF"
echo " Watch only:         sudo roamjev -iface $IF -observe"
echo " Try the simulator:  roamjev -sim"
echo " Dashboard:          http://127.0.0.1:8077"
echo " API key:            $KEY_FILE"
if [ "$SERVICE_INSTALLED" = true ]; then
  echo ""
  echo " Run as daemon:      sudo systemctl start roamjev@$IF"
  echo " View logs:          journalctl -u roamjev@$IF -f"
fi
echo ""
