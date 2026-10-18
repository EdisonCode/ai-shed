#!/bin/sh
# Signs the macOS binaries in dist with a Developer ID and notarizes them.
#
# macOS keeps a privacy grant, such as Full Disk Access, for a program signed
# by a developer by its identifier and team, and for an unsigned one by its
# exact build. Signed, the grant survives `shed update`.
#
# Environment:
#   MACOS_CERT_P12       the Developer ID Application certificate and key, base64
#   MACOS_CERT_PASSWORD  the .p12's password
#   APPLE_TEAM_ID        the team that owns the certificate (public: every
#                        signed binary names it)
#   NOTARY_KEY_P8        an App Store Connect API key, base64
#   NOTARY_KEY_ID        its key ID
#   NOTARY_ISSUER_ID     its issuer ID
#
# Never run it with -x: it would print the secrets.
set -eu

identifier=com.edisoncode.shed

for v in MACOS_CERT_P12 MACOS_CERT_PASSWORD APPLE_TEAM_ID NOTARY_KEY_P8 NOTARY_KEY_ID NOTARY_ISSUER_ID; do
	eval "val=\${$v:-}"
	if [ -z "$val" ]; then
		echo "sign-macos: $v is not set; a release is never published unsigned" >&2
		exit 1
	fi
done

work=$(mktemp -d)
keychain="$work/signing.keychain-db"
keychain_password=$(openssl rand -hex 16)
trap 'security delete-keychain "$keychain" 2>/dev/null; rm -rf "$work"' EXIT

printf '%s' "$MACOS_CERT_P12" | base64 --decode >"$work/cert.p12"
printf '%s' "$NOTARY_KEY_P8" | base64 --decode >"$work/notary.p8"

security create-keychain -p "$keychain_password" "$keychain"
security set-keychain-settings -lut 3600 "$keychain"
security unlock-keychain -p "$keychain_password" "$keychain"
security import "$work/cert.p12" -P "$MACOS_CERT_PASSWORD" -k "$keychain" -T /usr/bin/codesign
security set-key-partition-list -S apple-tool:,apple: -s -k "$keychain_password" "$keychain" >/dev/null
# codesign finds the identity only in a keychain on the search list. The
# current list is split into its paths on purpose.
# shellcheck disable=SC2046
security list-keychains -d user -s "$keychain" $(security list-keychains -d user | tr -d '"')

for bin in dist/shed-darwin-*; do
	codesign --force --timestamp --options runtime --identifier "$identifier" \
		--keychain "$keychain" --sign "Developer ID Application" "$bin"
	codesign --verify --strict --verbose=2 "$bin"
	# The grant is kept by this requirement: it must name the identifier and
	# the team, never the build.
	requirement=$(codesign --display --requirements - "$bin" 2>&1)
	case $requirement in
	*"identifier \"$identifier\""*"$APPLE_TEAM_ID"*) ;;
	*)
		echo "sign-macos: $bin's designated requirement does not name $identifier and team $APPLE_TEAM_ID:" >&2
		echo "$requirement" >&2
		exit 1
		;;
	esac
done

# Every binary is signed before any is notarized: a notarization can wait in
# Apple's queue for hours, and the keychain locks itself after an hour.
for bin in dist/shed-darwin-*; do
	# A bare binary cannot be stapled: Gatekeeper looks the ticket up online.
	zip="$work/$(basename "$bin").zip"
	ditto -c -k "$bin" "$zip"
	result=$(xcrun notarytool submit "$zip" --wait --output-format json \
		--key "$work/notary.p8" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER_ID")
	status=$(printf '%s' "$result" | jq -r .status)
	if [ "$status" != Accepted ]; then
		echo "sign-macos: notarization of $bin ended $status: $result" >&2
		id=$(printf '%s' "$result" | jq -r .id)
		xcrun notarytool log "$id" --key "$work/notary.p8" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER_ID" >&2 || true
		exit 1
	fi
	echo "sign-macos: $bin signed and notarized"
done
