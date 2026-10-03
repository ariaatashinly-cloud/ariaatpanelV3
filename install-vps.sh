#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
umask 077
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || { echo 'Linux amd64 is required.'; exit 1; }
command -v docker >/dev/null || { echo 'Install Docker Engine and its Compose plugin first.'; exit 1; }
docker compose version >/dev/null
command -v openssl >/dev/null || { echo 'Install openssl first.'; exit 1; }
if [[ ! -f .env ]]; then
 read -r -p 'Domain pointing to this server (vpn.example.com): ' aria_domain
 aria_domain="${aria_domain,,}"
 [[ "$aria_domain" =~ ^[a-z0-9][a-z0-9.-]*\.[a-z]{2,63}$ && "$aria_domain" != *..* ]] || { echo 'Invalid domain.'; exit 1; }
 read -r -s -p 'Private admin password (12+ characters, blank = generate): ' aria_password
 echo
 if [[ -z "$aria_password" ]]; then aria_password="$(openssl rand -hex 20)"; fi
 [[ ${#aria_password} -ge 12 && "$aria_password" != *"'"* && "$aria_password" != *$'\r'* ]] || { echo 'Use 12+ characters without apostrophe or carriage return.'; exit 1; }
 {
  printf 'ARIA_DOMAIN=%s\n' "$aria_domain"
  printf 'ARIA_ADMIN_USER=admin\n'
  printf "ARIA_ADMIN_PASSWORD='%s'\n" "$aria_password"
  printf 'ARIA_MODE=vps\n'
 } > .env
 chmod 0600 .env
 echo 'Credentials are saved privately in .env; keep a copy.'
else
 echo 'Existing .env retained.'
fi
docker compose -p ariaatashin -f compose.vps.yaml config --quiet
docker compose -p ariaatashin -f compose.vps.yaml up -d --build
echo 'Deployment started. Open https://YOUR_DOMAIN after DNS and TLS are ready.'
echo 'Allow TCP 80/443 and only the direct TCP/UDP ports you use. See INSTALL-VPS-FA.md.'
