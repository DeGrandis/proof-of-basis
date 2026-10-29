#!/bin/sh
# Starts three self-hosted S3 servers (Garage, SeaweedFS, RustFS) with test
# keys and a bucket named pob-backups on each, then writes the values to
# paste into Settings → Advanced backup to _secrets/settings.txt. The keys
# are made here, for these test servers only, and stay in _secrets/ (ignored
# by git).
#
#   ./setup.sh          start (makes keys the first time)
#   ./setup.sh down     stop and delete the servers and their data
set -eu
cd "$(dirname "$0")"
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: leave container paths alone

if [ "${1:-}" = down ]; then
  docker compose down -v
  rm -rf _secrets
  exit 0
fi

rand() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }
mkdir -p _secrets
if [ ! -f _secrets/garage.toml ]; then
  cat > _secrets/garage.toml <<EOF
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(rand 32)"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage.localhost"
EOF
  SW_KEY="SW$(rand 8)"; SW_SECRET="$(rand 20)"
  cat > _secrets/seaweedfs-s3.json <<EOF
{"identities": [{"name": "pob", "credentials": [{"accessKey": "$SW_KEY", "secretKey": "$SW_SECRET"}], "actions": ["Admin", "Read", "Write", "List", "Tagging"]}]}
EOF
  printf 'RUSTFS_ACCESS_KEY=RF%s\nRUSTFS_SECRET_KEY=%s\n' "$(rand 8)" "$(rand 20)" > _secrets/rustfs.env
fi

docker compose up -d

# Garage: a one-node layout, a key and the bucket.
for i in 1 2 3 4 5 6 7 8 9 10; do docker compose exec -T garage /garage status >/dev/null 2>&1 && break; sleep 1; done
NODE=$(docker compose exec -T garage /garage node id -q | cut -d@ -f1)
if docker compose exec -T garage /garage layout show | grep -q "No nodes"; then
  docker compose exec -T garage /garage layout assign -z dc1 -c 1G "$NODE" >/dev/null
  docker compose exec -T garage /garage layout apply --version 1 >/dev/null
fi
if [ ! -f _secrets/garage.key ]; then
  docker compose exec -T garage /garage key create pob > _secrets/garage.key
  docker compose exec -T garage /garage bucket create pob-backups >/dev/null
  docker compose exec -T garage /garage bucket allow --read --write --owner pob-backups --key pob >/dev/null
fi
GA_KEY=$(grep -i "key id" _secrets/garage.key | awk '{print $NF}')
GA_SECRET=$(grep -i "secret key" _secrets/garage.key | awk '{print $NF}')

# SeaweedFS and RustFS: make the bucket with a signed PUT.
SW_KEY=$(sed 's/.*"accessKey": "\([^"]*\)".*/\1/' _secrets/seaweedfs-s3.json)
SW_SECRET=$(sed 's/.*"secretKey": "\([^"]*\)".*/\1/' _secrets/seaweedfs-s3.json)
. ./_secrets/rustfs.env
mkbucket() { # url key secret
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    # (the body goes to stdout, not -o /dev/null, which Windows curl can't open)
    code=$(curl -s -w '\n%{http_code}' -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user "$2:$3" "$1/pob-backups" | tail -n1) || code=000
    case $code in 200|409) return 0 ;; esac
    sleep 1
  done
  echo "couldn't make the bucket at $1 (HTTP $code)" >&2
  return 1
}
mkbucket http://127.0.0.1:8333 "$SW_KEY" "$SW_SECRET"
mkbucket http://127.0.0.1:9100 "$RUSTFS_ACCESS_KEY" "$RUSTFS_SECRET_KEY"

cat > _secrets/settings.txt <<EOF
Settings → Advanced backup → S3-compatible storage. Tick "Bucket in the path"
for all three. The app must be on the pob-s3 Docker network:
  docker network connect pob-s3 <app container>

Garage
  Endpoint  http://garage:3900
  Bucket    pob-backups
  Region    garage
  Access key ID  $GA_KEY
  Secret key     $GA_SECRET

SeaweedFS
  Endpoint  http://seaweedfs:8333
  Bucket    pob-backups
  Region    us-east-1
  Access key ID  $SW_KEY
  Secret key     $SW_SECRET

RustFS (console at http://127.0.0.1:9101, log in with the same key and secret)
  Endpoint  http://rustfs:9000
  Bucket    pob-backups
  Region    us-east-1
  Access key ID  $RUSTFS_ACCESS_KEY
  Secret key     $RUSTFS_SECRET_KEY
EOF
echo "Ready. Values to paste: $(pwd)/_secrets/settings.txt"
