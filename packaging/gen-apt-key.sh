#!/bin/sh
# Generates the PGP key used to sign the apt repository, and prints the
# private key to add as the APT_GPG_KEY GitHub secret. The public key is
# committed to the gh-pages branch by the release workflow (key.asc).
#
# Usage:
#   ./packaging/gen-apt-key.sh
#
# Then add the printed private key as the APT_GPG_KEY repository secret.
set -eu

KEY_NAME="Ferngeist Gateway (apt repo signing) <maintainers@ferngeist.local>"
KEY_FILE="$(mktemp -d)/ferngeist-apt.gpg"

# Batch-generate an Ed25519 keypair with no passphrase (CI needs non-interactive
# signing). Valid for 5 years, no expiration concerns for a repo signing key.
# Note: --quick-generate-key (not --gen-key) is the batch-compatible form.
gpg --batch --passphrase '' --quick-generate-key "$KEY_NAME" ed25519 sign 5y

# Export the private key, armored, for the GitHub secret.
gpg --batch --export-secret-key --armor "$KEY_NAME" > "$KEY_FILE"

echo
echo "=== Add this as the APT_GPG_KEY repository secret (Settings -> Secrets -> Actions) ==="
echo
cat "$KEY_FILE"
echo
echo "=== Done. The public key will be committed automatically on the next release. ==="
