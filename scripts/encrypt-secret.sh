#!/bin/sh
set -eu
if [ "$#" -ne 2 ]; then
  echo 'usage: encrypt-secret.sh CONFIG_ENV CONTAINERNODE_CHECKOUT' >&2
  exit 2
fi
config_file=$1
infra_dir=$2
target="$infra_dir/infrastructure/containernode/lifeblood/lifeblood-config.secret.sops.yaml"
if [ ! -d "$(dirname "$target")" ]; then
  echo 'Install deploy/kubernetes in the infrastructure repository first.' >&2
  exit 1
fi
umask 077
staging=$(mktemp)
trap 'rm -f "$staging"' EXIT HUP INT TERM
kubectl create secret generic lifeblood-config --namespace lifeblood \
  --from-env-file="$config_file" --dry-run=client -o yaml > "$staging"
sops --config "$infra_dir/.sops.yaml" --encrypt --filename-override "$target" "$staging" > "$target.tmp"
mv "$target.tmp" "$target"
echo "Saved encrypted Kubernetes Secret to $target"
