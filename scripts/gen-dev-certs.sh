#!/usr/bin/env bash
# gen-dev-certs.sh — generate a self-signed dev PKI for mando-agent mTLS.
#
# FOR M2 TESTING ONLY. Not a real CA: one CA signs both the server (mando-agent) and the
# client (mando-reconciler / control plane). Replace with your real PKI before production.
#
# Usage: scripts/gen-dev-certs.sh [OUT_DIR] [SERVER_HOST]
#   OUT_DIR      where to write the PKI (default: secrets/fleet-tls, gitignored)
#   SERVER_HOST  DNS name or IP the reconciler will dial (default: 127.0.0.1)
#
# Produces the exact filenames the fleet_agent Ansible role and the binaries expect:
#   server.crt server.key client-ca.crt   (mando-agent)
#   reconciler.crt reconciler.key server-ca.crt  (mando-reconciler)
set -euo pipefail

OUT="${1:-secrets/fleet-tls}"
HOST="${2:-127.0.0.1}"
DAYS=825

mkdir -p "$OUT"
cd "$OUT"

if [[ "$HOST" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  SAN="IP:${HOST},DNS:localhost,IP:127.0.0.1"
else
  SAN="DNS:${HOST},DNS:localhost,IP:127.0.0.1"
fi

echo "gen-dev-certs: CA"
openssl req -x509 -newkey rsa:4096 -nodes -keyout ca.key -out ca.crt -days "$DAYS" \
  -subj "/CN=fleet-dev-ca" 2>/dev/null

echo "gen-dev-certs: server (mando-agent), SAN=${SAN}"
openssl req -newkey rsa:4096 -nodes -keyout server.key -out server.csr \
  -subj "/CN=mando-agent" 2>/dev/null
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days "$DAYS" \
  -extfile <(printf "subjectAltName=%s\n" "$SAN") 2>/dev/null

echo "gen-dev-certs: client (mando-reconciler)"
openssl req -newkey rsa:4096 -nodes -keyout reconciler.key -out reconciler.csr \
  -subj "/CN=mando-reconciler" 2>/dev/null
openssl x509 -req -in reconciler.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out reconciler.crt -days "$DAYS" 2>/dev/null

cp ca.crt client-ca.crt
cp ca.crt server-ca.crt
rm -f server.csr reconciler.csr ca.srl
chmod 600 ./*.key

echo "gen-dev-certs: wrote dev PKI to ${OUT}"
