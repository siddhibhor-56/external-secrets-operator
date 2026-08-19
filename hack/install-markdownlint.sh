#!/bin/sh
# Install markdownlint-cli2 globally via npm.
# If npm is missing, install Node.js/npm with dnf or yum (requires root).
#
# Invoked by:
#   - openshift/release test_binary_build_commands
#   - hack/Dockerfile.markdownlint
#
# Override version: MARKDOWNLINT_CLI2_VERSION=<ver> ./hack/install-markdownlint.sh
set -eu

MARKDOWNLINT_CLI2_VERSION="${MARKDOWNLINT_CLI2_VERSION:-0.18.1}"

install_nodejs() {
	if command -v npm >/dev/null 2>&1; then
		return 0
	fi
	if [ "$(id -u)" -ne 0 ]; then
		echo "npm is not installed; re-run as root or install Node.js/npm first." >&2
		exit 1
	fi
	if command -v dnf >/dev/null 2>&1; then
		dnf -y module reset nodejs || true
		# module streams differ by distro; try 22 then 20, then default packages.
		dnf -y module enable nodejs:22 || dnf -y module enable nodejs:20 || true
		dnf -y install nodejs npm
		return 0
	fi
	if command -v yum >/dev/null 2>&1; then
		yum -y install nodejs npm
		return 0
	fi
	echo "Unable to install Node.js/npm: no dnf/yum package manager found." >&2
	exit 1
}

install_nodejs
npm install -g "markdownlint-cli2@${MARKDOWNLINT_CLI2_VERSION}"
command -v markdownlint-cli2
npm list -g --depth=0 "markdownlint-cli2@${MARKDOWNLINT_CLI2_VERSION}"
