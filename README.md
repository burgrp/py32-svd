# PY32 CMSIS-SVD files

This repository contains CMSIS-SVD descriptions used to generate TinyGo's
`device/py32` package. The files in `svd/` are generated from official Puya
CMSIS device family packs and are committed so building TinyGo does not require
network access.

## Updating

The updater requires Go 1.22 or newer and otherwise uses only the Go standard
library:

```sh
go run ./cmd/update
```

Pack URLs and known SHA-256 checksums are kept in `packs.json`. All configured
packs are required. The updater downloads and validates every pack before it
atomically replaces `svd/`; a failed download, checksum, archive, XML, or name
collision leaves the previous output unchanged.

CMSIS `.pack` files are ZIP archives. Every `.svd` found recursively in every
pack is published using a lowercase vendor filename. If two packs contain the
same output filename, they must contain identical bytes. A deterministic
`manifest.json` records the origin and hashes of all generated files.

After committing an update here, update TinyGo's `lib/py32-svd` submodule and
run:

```sh
make gen-device-py32
```

Run the updater twice before committing. The second run must produce no diff.

## Current upstream issue

As of 2026-08-05, the CMSIS pack index advertises
`Puya.PY32F3xx_DFP.1.0.0.pack`, but both Puya's canonical URL and Keil's pack
CDN return HTTP 404. The unavailable pack is therefore disabled by omission
from `packs.json`. Its canonical URL is
`https://www.puyasemi.com/uploadfiles/Puya.PY32F3xx_DFP.1.0.0.pack`.

When the archive is restored, download it, verify its contents, add it back to
`packs.json` with its SHA-256, and regenerate `svd/`. All packs that remain in
the active configuration are mandatory.

## Register descriptions

Puya's SVD files contain few descriptions. The packs also contain `.SFR` files
paired with SVDs by basename (for example, `PY32F030xx.SFR` and
`PY32F030xx.svd`). Inspection of the F0 pack shows these are compiled Keil
`SfrCC2` binaries, not XML. They reference an original `.sfd` source and contain
human-readable interrupt, register, and bitfield descriptions among binary
data. Enrichment will therefore require a safe decoder for this format or
access to the original `.sfd` files; it cannot be implemented as a simple XML
merge.

Description enrichment is intentionally deferred. The updater keeps extraction
and publication separate so a future transformation can decode and merge this
metadata before SVD validation without changing download or atomic-update
behavior.

## Tests

Tests create small CMSIS packs locally and never use the network:

```sh
go test ./...
go test -race ./...
```

They cover recursive extraction, mixed-case extensions, checksums, malformed
archives and XML, unsafe ZIP entries, output collisions, resource limits,
deterministic manifests, HTTP failures, and preservation of the old output on
failure.