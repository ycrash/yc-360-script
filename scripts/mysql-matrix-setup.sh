#!/bin/sh
# Prepares the host for compose.mysql.yaml: one error-log directory per server,
# and a throwaway CA and server certificate for the verified-TLS server.
#
#   scripts/mysql-matrix-setup.sh
#   docker compose -f compose.mysql.yaml up -d --wait
#
# The certificate is generated here rather than committed: a private key in the
# repository is what gitleaks exists to flag, even a test one. It names 127.0.0.1
# and localhost as subject alternative names, which Go's verifier requires - the
# server's auto-generated certificate carries a fixed common name and no SAN.
set -eu

for server in my80 my80old my84 my9 percona84 my84-replica my84-perms; do
	mkdir -p "/tmp/yc-mylogs/$server"
	chmod 777 "/tmp/yc-mylogs/$server"
done

tls=/tmp/yc-mytls
mkdir -p "$tls"

if [ ! -s "$tls/server-cert.pem" ]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
		-subj "/CN=yc matrix test CA" \
		-keyout "$tls/ca-key.pem" -out "$tls/ca.pem" 2>/dev/null

	openssl req -newkey rsa:2048 -nodes \
		-subj "/CN=localhost" \
		-keyout "$tls/server-key.pem" -out "$tls/server.csr" 2>/dev/null

	printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\n' >"$tls/san.ext"

	openssl x509 -req -days 3650 -in "$tls/server.csr" \
		-CA "$tls/ca.pem" -CAkey "$tls/ca-key.pem" -CAcreateserial \
		-extfile "$tls/san.ext" -out "$tls/server-cert.pem" 2>/dev/null

	rm -f "$tls/server.csr" "$tls/san.ext"
fi

# The server runs as its own uid inside the container and must read the key.
chmod 755 "$tls"
chmod 644 "$tls"/*.pem
