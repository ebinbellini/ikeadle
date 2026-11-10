#!/usr/bin/env bash
#
# generate-favicons.sh -- build the favicon set for IKEAdle.
#
#   source : utils/IKEAdle favicon.svg     (blue tile with a yellow ellipse and DLE)
#   output : web_static/icons/
#
#     favicon.svg              vector favicon
#     favicon.ico              16+32+48 px
#     favicon-16x16.png
#     favicon-32x32.png
#     favicon-48x48.png
#     favicon-96x96.png
#     apple-touch-icon.png     180 px on IKEA blue
#     icon-192.png             IKEA blue tile (Android / manifest)
#     icon-512.png             IKEA blue tile
#     icon-maskable-512.png    icon shrunk into the 80% maskable safe zone
#     manifest.json            web app manifest, linked from server/templates/layout.html
#
# Requires: rsvg-convert (librsvg), ImageMagick 7 (`magick`), scour (optional).
#
# Usage: utils/generate-favicons.sh
#
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(dirname -- "$script_dir")

src_svg="$script_dir/IKEAdle favicon.svg"
out_dir="$root_dir/web_static/icons"

brand_blue='#0058a3'
raster=1024           # internal render size; every icon is downscaled from this

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for tool in rsvg-convert magick; do
	command -v "$tool" >/dev/null 2>&1 || die "'$tool' not found in PATH"
done
[ -f "$src_svg" ] || die "source SVG not found: $src_svg"

mkdir -p "$out_dir"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ------------------------------------------------------------------ clean SVG
# Strip editor metadata so the shipped file is small and easy to diff. The SVG
# keeps whatever width/height/viewBox the source has; rsvg-convert scales it to
# the raster size below, and browsers scale the vector via its viewBox. Do not
# inject extra width/height attributes here: the source may already define them,
# which would produce an invalid SVG with duplicate attributes.
if command -v scour >/dev/null 2>&1; then
	scour -i "$src_svg" -o "$work/clean.svg" \
		--enable-viewboxing --remove-metadata --indent=none >/dev/null
else
	printf 'warning: scour not found -- shipping the unoptimised SVG\n' >&2
	cp "$src_svg" "$work/clean.svg"
fi

# ----------------------------------------------------------------- rasterise
rsvg-convert -w "$raster" -h "$raster" "$work/clean.svg" -o "$work/full.png"

# ------------------------------------------------------------------- helpers
# square <src.png> <px> <out.png> [background]
#
# Resizes the square artwork and, when a background is given, flattens the
# transparent rounded corners onto IKEA blue (what iOS/Android expect).
square() {
	local src=$1 px=$2 out=$3 bg=${4:-none}
	if [ "$bg" = none ]; then
		magick "$src" -filter Lanczos -resize "${px}x${px}" -strip "$out"
	else
		magick "$src" -filter Lanczos -resize "${px}x${px}" \
			-background "$bg" -gravity center -extent "${px}x${px}" \
			-alpha remove -alpha off -strip "$out"
	fi
	printf '  %-24s %sx%s\n' "${out#"$out_dir/"}" "$px" "$px"
}

# -------------------------------------------------------------------- outputs
printf 'Generating favicons in %s\n' "${out_dir#"$root_dir/"}"

cp "$work/clean.svg" "$out_dir/favicon.svg"
printf '  %-24s vector\n' favicon.svg

square "$work/full.png" 16  "$out_dir/favicon-16x16.png"
square "$work/full.png" 32  "$out_dir/favicon-32x32.png"
square "$work/full.png" 48  "$out_dir/favicon-48x48.png"
square "$work/full.png" 96  "$out_dir/favicon-96x96.png"

square "$work/full.png" 180 "$out_dir/apple-touch-icon.png" "$brand_blue"
square "$work/full.png" 192 "$out_dir/icon-192.png" "$brand_blue"
square "$work/full.png" 512 "$out_dir/icon-512.png" "$brand_blue"

# Maskable icons must keep their important content inside the middle 80%.
magick "$work/full.png" -filter Lanczos -resize 410x410 \
	-background "$brand_blue" -gravity center -extent 512x512 \
	-alpha remove -alpha off -strip "$out_dir/icon-maskable-512.png"
printf '  %-24s %sx%s\n' icon-maskable-512.png 512 512

# Legacy multi-resolution icon.
magick "$out_dir/favicon-16x16.png" "$out_dir/favicon-32x32.png" \
	"$out_dir/favicon-48x48.png" -strip "$out_dir/favicon.ico"
printf '  %-24s %s\n' favicon.ico \
	"$(magick identify -format '%w ' "$out_dir/favicon.ico" | sed 's/ $//; s/ /+/g')"

cat >"$out_dir/manifest.json" <<'JSON'
{
  "name": "IKEAdle",
  "short_name": "IKEAdle",
  "description": "Gissa priset på varor ur IKEA-sortimentet.",
  "lang": "sv",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#ffffff",
  "theme_color": "#0058a3",
  "icons": [
    { "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png" },
    {
      "src": "/icons/icon-maskable-512.png",
      "sizes": "512x512",
      "type": "image/png",
      "purpose": "maskable"
    }
  ]
}
JSON
printf '  %-24s manifest\n' manifest.json

cat <<'HTML'

Reference them from the page <head> (done in server/templates/layout.html):

  <link rel="icon" href="/icons/favicon.ico" sizes="32x32">
  <link rel="icon" href="/icons/favicon.svg" type="image/svg+xml">
  <link rel="apple-touch-icon" href="/icons/apple-touch-icon.png">
  <link rel="manifest" href="/icons/manifest.json">
  <meta name="theme-color" content="#0058a3">
HTML
