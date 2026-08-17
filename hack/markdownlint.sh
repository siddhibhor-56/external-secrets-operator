#!/bin/sh
# Wrapper around markdownlint-cli2.
# Reads .markdownlint-cli2.yaml from the current working directory.
# Forwards all CLI args (e.g. --fix).
set -eu

handle_exit() {
	if [ "$1" != "0" ]; then
		cat <<'EOF'

Markdown lint failed. From the repository root, run:

  make lint-markdown

To auto-fix many style issues:

  make lint-markdown-fix

EOF
	fi
}

trap 'handle_exit $?' EXIT

if ! command -v markdownlint-cli2 >/dev/null 2>&1; then
	echo "markdownlint-cli2 not found on PATH." >&2
	echo "Install: ./hack/install-markdownlint.sh" >&2
	echo "Or: make markdownlint-image && make lint-markdown" >&2
	exit 1
fi

markdownlint-cli2 "$@"
