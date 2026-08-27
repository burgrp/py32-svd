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
`manifest.json` records the origin and hashes of all generated files. For SVDs
enriched from CMSIS device headers, it also records the archive paths of the
headers and the number of inserted `enumeratedValue` elements.

Before validation and publication, the updater applies small, deterministic
corrections for known vendor metadata inconsistencies. Puya's packs do not use
`groupName` consistently for GPIO ports: identical register blocks may become
`GPIOA_Type`, `GPIOB_Type`, and so on in SVD consumers. The updater assigns the
common `GPIO` group to every GPIO port after verifying that all explicit port
layouts are compatible. A port may omit registers present in the richest port,
but conflicting offsets or register shapes abort the update. The manifest
hashes describe these patched, published bytes rather than the raw ZIP entries.

The three-bit `RCC.ICSCR.HSI_FS` field receives a `Freq24MHz` value using the
family-specific encoding specified by the PY32 reference manuals: 4 for the
common layout and 3 for PY32F032. Adding this semantic value before header
enrichment prevents CMSIS bit-component macros such as `HSI_FS_2` from being
published as if they were field choices.

### Header-derived field values

Puya's SVD files describe register fields but generally omit their
`enumeratedValues`. The official packs also contain CMSIS C device headers with
definitions such as `RCC_CFGR_SWS_HSE`. Before publication, the updater matches
each concrete device header to the SVD with the longest matching device-family
prefix and adds these semantic values to the corresponding SVD field. This
keeps PY32-specific source repair here while allowing ordinary CMSIS-SVD tools,
including TinyGo's generic generator, to consume the result unchanged.

The extraction is deliberately conservative:

- only object-like integer macros are considered;
- literals, aliases, parentheses, integer casts, constant wrappers, shifts,
	arithmetic, and bitwise expressions are evaluated with checked 64-bit
	arithmetic;
- function-like macros, unresolved identifiers, unsupported C syntax,
	recursive aliases, and overflowing expressions are ignored; if any repeated
	or conditional definition of a name cannot be evaluated, that name is
	ignored entirely;
- a definition must match `PERIPHERAL_REGISTER_FIELD_VALUE`, using either the
	SVD peripheral name or `groupName`;
- the header's corresponding `_Pos` and `_Msk` helpers must exactly match the
	SVD field's bit offset and mask;
- `_Pos` and `_Msk` helpers are ignored, while numbered components such as
	`_0` and `_1` are retained because they are useful CMSIS-compatible Go
	constants, except where a deterministic correction supplies semantic values;
- register-positioned C values must fit wholly inside the matched SVD field and
	are shifted to field-local SVD values;
- conflicting definitions within one header or between device variants are
	omitted; and
- fields that already contain `enumeratedValues`, and fields that inherit via
	`derivedFrom`, are left untouched.

Descriptions are copied only from comments attached to accepted semantic
macros and only when descriptions from all associated variants agree. Hex-value
comments on mask helpers are not treated as documentation.
Output ordering, XML escaping, indentation, and LF/CRLF line endings are
deterministic, and a second enrichment pass is a no-op.

At the currently pinned pack revisions, 39 of 41 SVDs have matching device
headers. The F0 pack publishes `PY32F001xx.svd` and `PY32F001Cxx.svd`, and its
common header refers to a `py32f001x4.h`, but that device header is absent from
the archive. Those two SVDs therefore remain unenriched rather than borrowing
definitions from another family; their manifest entries intentionally have no
`headers` field.

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

SFR-derived register-description enrichment is intentionally deferred. The
updater keeps extraction and publication separate so a future transformation
can decode and merge this metadata before SVD validation without changing
download or atomic-update behavior.

## Tests

Tests create small CMSIS packs locally and never use the network:

```sh
go test ./...
go test -race ./...
```

They cover recursive extraction, mixed-case extensions, checksums, malformed
archives and XML, unsafe ZIP entries, output collisions, resource limits,
deterministic manifests, GPIO patch safety and idempotence, header parsing and
checked expression evaluation, header/SVD association, conflicting definitions,
field-bound validation, XML enrichment and idempotence, HTTP failures, and
preservation of the old output on failure.
