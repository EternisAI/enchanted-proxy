#!/bin/sh
set -eu
# The transparent proxy status port is 9101
echo "Waiting for the transparent proxy to come up"
until nc -w1 -z 127.0.0.1 9101; do
  sleep 1
done
# Route all egress traffic to the transparent proxy on the localhost
echo "Setting up default route"
ip route add local default dev lo
# Launch the application binary
echo "Starting server"
exec server
