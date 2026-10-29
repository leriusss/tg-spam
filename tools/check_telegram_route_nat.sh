#!/bin/sh
# Runner-only demonstration: --resolve/getpeername cannot prove bypass of OUTPUT DNAT.
# All mutations are confined to a NEW, disconnected network namespace.
set -eu
if [ "${1:-}" != --isolated ]; then
    exec sudo unshare --net sh "$0" --isolated
fi
ip link set lo up
scratch=$(mktemp -d /tmp/tg-route-nat.XXXXXXXX)
server_pid=
cleanup() {
    if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
    rm -rf "$scratch"
}
trap cleanup EXIT INT TERM
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=api.telegram.org \
    -addext subjectAltName=DNS:api.telegram.org -keyout "$scratch/key.pem" -out "$scratch/cert.pem" 2>/dev/null
cat > "$scratch/server.py" <<'PY'
import http.server, ssl, sys, threading
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(sys.argv[1], sys.argv[2])
def serve(ip, label):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = label.encode()
            self.send_response(200)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        def log_message(self, *_): pass
    server = http.server.HTTPServer((ip, 443), Handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()
for ip, label in [('127.0.0.2', 'primary'), ('127.0.0.3', 'temporary-address')]:
    threading.Thread(target=serve, args=(ip, label), daemon=True).start()
threading.Event().wait()
PY
python3 "$scratch/server.py" "$scratch/cert.pem" "$scratch/key.pem" &
server_pid=$!
probe() {
    curl --silent --show-error --noproxy '*' --max-time 2 --cacert "$scratch/cert.pem" \
        --resolve api.telegram.org:443:127.0.0.2 https://api.telegram.org/
}
ready=false
for attempt in 1 2 3 4 5; do
    if result=$(probe 2>/dev/null) && [ "$result" = primary ]; then ready=true; break; fi
    sleep 0.2
done
[ "$ready" = true ]
printf 'before DNAT, explicit --resolve: %s\n' "$result"
iptables -t nat -A OUTPUT -d 127.0.0.2/32 -p tcp --dport 443 \
    -j DNAT --to-destination 127.0.0.3:443
result=$(probe)
[ "$result" = temporary-address ]
printf 'after DNAT, SAME explicit --resolve: %s (TLS still verified)\n' "$result"
iptables -t nat -nvL OUTPUT
printf 'PASS: server identity/payload and NAT counters prove the actual path; --resolve alone does not.\n'
