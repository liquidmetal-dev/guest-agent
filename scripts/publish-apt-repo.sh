#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'USAGE' >&2
usage: publish-apt-repo.sh <dist-dir> <apt-repo-dir>

Publishes guest-agent .deb artifacts from a GoReleaser dist directory into a
checked-out apt repository, regenerates stable/main metadata, signs it, and
commits any changes.
USAGE
}

if [ "$#" -ne 2 ]; then
	usage
	exit 2
fi

dist_dir=$1
repo_dir=$2

if [ ! -d "$dist_dir" ]; then
	echo "dist directory does not exist: $dist_dir" >&2
	exit 1
fi

if [ ! -d "$repo_dir/.git" ]; then
	echo "apt repository checkout does not exist: $repo_dir" >&2
	exit 1
fi

for tool in apt-ftparchive dpkg-deb gzip gpg sha256sum; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "required tool not found: $tool" >&2
		exit 1
	fi
done

repo_root=$(cd "$repo_dir" && pwd)
pool_dir="$repo_root/pool/main/g/guest-agent"

mkdir -p "$pool_dir"

shopt -s nullglob
debs=("$dist_dir"/*.deb)
shopt -u nullglob

if [ "${#debs[@]}" -eq 0 ]; then
	echo "no .deb artifacts found in $dist_dir" >&2
	exit 1
fi

published=0
for deb in "${debs[@]}"; do
	package=$(dpkg-deb -f "$deb" Package)
	arch=$(dpkg-deb -f "$deb" Architecture)

	if [ "$package" != "guest-agent" ]; then
		echo "unexpected package in $deb: $package" >&2
		exit 1
	fi

	case "$arch" in
		amd64 | arm64) ;;
		*)
			echo "unexpected architecture in $deb: $arch" >&2
			exit 1
			;;
	esac

	dest="$pool_dir/$(basename "$deb")"
	if [ -e "$dest" ]; then
		if cmp -s "$deb" "$dest"; then
			echo "already published: $(basename "$deb")"
			continue
		fi

		echo "refusing to replace existing package with different bytes: $dest" >&2
		echo "existing: $(sha256sum "$dest" | awk '{print $1}')" >&2
		echo "new:      $(sha256sum "$deb" | awk '{print $1}')" >&2
		exit 1
	fi

	cp "$deb" "$dest"
	echo "published: $(basename "$deb")"
	published=$((published + 1))
done

cd "$repo_root"

gpg_args=(--batch --yes --pinentry-mode loopback)
if [ -n "${APT_GPG_PASSPHRASE:-}" ]; then
	gpg_args+=(--passphrase "$APT_GPG_PASSPHRASE")
fi
if [ -n "${APT_GPG_KEY_ID:-}" ]; then
	gpg_args+=(-u "$APT_GPG_KEY_ID")
	gpg_key_selector=("$APT_GPG_KEY_ID")
else
	gpg_key_selector=()
fi

gpg --batch --yes --armor --output liquidmetal-archive-keyring.asc --export "${gpg_key_selector[@]}"
gpg --batch --yes --output liquidmetal-archive-keyring.gpg --export "${gpg_key_selector[@]}"

if [ "$published" -eq 0 ] && [ -f dists/stable/Release ] && git diff --quiet -- .; then
	echo "apt repository already up to date"
	exit 0
fi

for arch in amd64 arm64; do
	binary_dir="dists/stable/main/binary-$arch"
	mkdir -p "$binary_dir"
	apt-ftparchive --arch "$arch" packages pool/main >"$binary_dir/Packages"
	gzip -9c "$binary_dir/Packages" >"$binary_dir/Packages.gz"
done

apt-ftparchive \
	-o APT::FTPArchive::Release::Origin="LiquidMetal" \
	-o APT::FTPArchive::Release::Label="LiquidMetal" \
	-o APT::FTPArchive::Release::Suite="stable" \
	-o APT::FTPArchive::Release::Codename="stable" \
	-o APT::FTPArchive::Release::Architectures="amd64 arm64" \
	-o APT::FTPArchive::Release::Components="main" \
	release dists/stable >dists/stable/Release

gpg "${gpg_args[@]}" --clearsign -o dists/stable/InRelease dists/stable/Release
gpg "${gpg_args[@]}" --detach-sign -o dists/stable/Release.gpg dists/stable/Release

if ! git diff --quiet -- .; then
	git add dists pool liquidmetal-archive-keyring.asc liquidmetal-archive-keyring.gpg
	git commit -m "Publish guest-agent apt packages"
	git push
	echo "apt repository updated; new packages copied: $published"
else
	echo "apt repository already up to date"
fi
