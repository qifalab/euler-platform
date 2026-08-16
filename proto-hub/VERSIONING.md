# proto-hub — API Versioning & Deprecation Policy (M-5.3, R14, 03§9.4)

The IDL in `proto-hub/` is the single source of truth for every product's
OpenAPI surface. `buf` (`breaking: WIRE_JSON`, see `buf.yaml`) is the real
gate; `tools/check-proto-breaking.py` is the source-only CI guard that catches
the same class of wire-breaking changes where `buf` is not installed.

## 1. Versioning

- Every proto package lives under a **version directory**: `starcloud/{domain}/v1/*.proto`.
  An unversioned package cannot be retired compatibly — the checker rejects it.
- Versions are `vN` (stable) and `vNbetaM` (preview). The directory name is the
  wire version; there is no per-file version field.
- **Additive-only within a major version.** A `v1` may gain fields, messages, and
  rpcs, and enum values appended at higher numbers. It may not remove, renumber,
  or retype anything. The checker pins field numbers against the baseline
  (`proto-hub/.breaking-baseline.json`) so a removal or retype is a CI failure.

## 2. Deprecation (R14: a breaking change needs architecture-committee sign-off)

A field, message, rpc, or enum value is retired in **two steps**, never one:

1. **Deprecate.** Add a `// Deprecated:` comment immediately above the symbol,
   state the removal window, and name the replacement. A deprecated symbol
   stays in place and on the wire for **at least one full major version**.

   ```proto
   // Deprecated: replaced by instance_type_v2 (richer spec model); removed in v2.
   string instance_type = 7;
   ```

2. **Remove.** Only after the deprecation window, in a new major version
   (`v2/`). Removal within `v1` is a breaking change.

`tools/check-proto-breaking.py` treats a field whose number vanished from the
baseline as a failure regardless of a `// Deprecated:` trail — deprecation is a
*notice* mechanism, not a license to break `v1`. The baseline diff is the hard
gate; the comment is the audit trail a reviewer expects to see alongside it.

## 3. The baseline

`proto-hub/.breaking-baseline.json` records the field-number map per message at
the last clean run. The checker writes it on first run and diffs against it on
every subsequent run. A genuine removal/retype must be accompanied by a major
version bump; otherwise CI fails. Regenerate after an intentional, approved
breaking change lands in a new major version.

## 4. Enforcement map

| Change                              | Detected by                          | Gate        |
|-------------------------------------|---------------------------------------|-------------|
| Field number removed / retyped      | baseline diff (`check-proto-breaking`) | CI failure  |
| Field number reused for a new type  | raw reuse scan (`check-proto-breaking`) | CI failure  |
| `required` keyword (proto3 forbids) | required scan                          | CI failure  |
| Package not under a version dir     | versioned-package check                | CI failure  |
| Deprecated symbol removed in v1     | baseline diff                          | CI failure  |
| Any wire-incompatible change         | `buf breaking WIRE_JSON` (when buf present) | CI failure |

A change that passes all of the above is additive and ships without a version
bump. Anything that does not is a breaking change and requires
architecture-committee sign-off (R14) plus a new major version.
