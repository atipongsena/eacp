# Sourced by the examples: loads examples/.env without printing it.
env_file="$(dirname "${BASH_SOURCE[0]}")/.env"
if [ ! -f "$env_file" ]; then
	echo "examples/.env is missing: run examples/setup.sh first" >&2
	exit 1
fi
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
