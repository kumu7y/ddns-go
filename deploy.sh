#!/bin/bash
set -e

SERVER1="kumu7y@kumu7y.icu"
SERVER2="kumu7y@hz.kumu7y.icu"
BINARY="DDNS-linux-arm64"
REMOTE_DIR="/opt/DDNS"

echo "=== Building for Linux ARM64 ==="
GOOS=linux GOARCH=arm64 go build -o "$BINARY" ddns.go
echo "Build complete: $BINARY"

echo ""
echo "=== Deploying to $SERVER1 ==="
scp "$BINARY" "$SERVER1:$REMOTE_DIR/"
ssh "$SERVER1" "chmod +x $REMOTE_DIR/$BINARY && systemctl restart ddns && sleep 2 && systemctl status ddns --no-pager -l"

echo ""
echo "=== Deploying to $SERVER2 ==="
scp "$BINARY" "$SERVER2:$REMOTE_DIR/"
ssh "$SERVER2" "chmod +x $REMOTE_DIR/$BINARY && systemctl restart ddns && sleep 2 && systemctl status ddns --no-pager -l"

echo ""
echo "=== Deployment complete ==="
