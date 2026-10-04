#!/bin/sh
set -eu
config="${CLIPROXY_CONFIG_PATH:-/config/config.yaml}"
/usr/local/bin/configure-quota-balancer -config "$config" -policy /etc/quota-balancer.yaml
exec /CLIProxyAPI/CLIProxyAPI -config "$config" "$@"
