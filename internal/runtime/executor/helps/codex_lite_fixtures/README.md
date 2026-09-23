# Codex reserved Lite tool definitions

The JSON files are generated artifacts. Do not shorten descriptions, trim schemas,
add constraints, or regenerate expected values from CPA's Go implementation.

CPA injects the complete `0.156.0/image_gen.json` and `0.156.0/web.json` namespace
objects. The independently exported 0.154 image definition is also recognized so
an existing native client declaration is preserved. The web exports are identical
between these two revisions. These are client protocol baselines, not a claim
that every upstream model accepts every version.

## Provenance and reproduction

- `rust-v0.156.0`: `fe74a774532af67b5a4a3dec03ce9469e17f89af`
- `rust-v0.154.0`: `6b9826e3aa83b1a5947db50f4332cb9c65f1b340`
- Rust: 1.95.0, as pinned by both upstream source revisions.
- Each version's `provenance.json` records source hashes (including the complete
  official Cargo.lock), exported artifact hashes and the isolated export lock hash.
- Every input source hash was independently compared with the file at its official
  GitHub commit. Every published dependency version and checksum in the export
  lockfile was checked against the corresponding official Cargo.lock.

With a checkout of the pinned source and Rust installed:

```sh
python3 generate.py /path/to/codex-0.156.0/codex-rs /tmp/cpa-tool-export 0.156.0
python3 generate.py /path/to/codex-0.154.0/codex-rs /tmp/cpa-tool-export 0.154.0
```

The script compiles an isolated Rust exporter. It copies the actual ImagegenArgs,
AbsolutePathBuf, SearchCommands, schema generation functions, schema parser and
compaction modules, response tool structs, complete Markdown descriptions and
Lite serializer from the supplied source. Wire-producing Rust sections are not
rewritten. Only module imports and exporter scaffolding are added. Runtime-only
types that are never constructed in these namespace specs are represented by
unused aliases; output_schema is `None` and skipped by the official serializer.
This avoids compiling the CLI's unrelated execution, UI and networking subsystems.

The initial build adapts the official lockfile to the isolated exporter root;
all retained dependency identities are then checked against the official lockfile.
Artifact export runs with `cargo run --locked`. Source files are never modified.
Reproduction checks each input against the recorded source hashes and fails on
another revision. The exporter never contacts a model or tool service.

The resulting image schema contains neither maxItems nor numeric bounds, but
does retain the complete function description and derived absolute-path description.
The web schema includes all SearchCommands fields, with no mandatory search_query,
and is parsed through the official no-compaction path.

## Legacy and conflict handling

`legacy/` contains the exact erroneous CPA templates from commit
`a47d55b2e6b76751ed7d2cff84aca130ea50ee90`, solely for recognizing the known defect.
They are never used for new injection.

In Lite mode, verified declarations are retained byte-for-byte. Exact legacy
templates can be repaired only in fresh contexts without a previous_response_id,
identified additional_tools items, assistant history, reasoning or tool history.
Existing contexts get a request-scoped 400 that asks for a new context or complete
rebuild/replay; the proxy never mutates historical declarations under an old ID.
Unknown conflicting reserved definitions also get a request-scoped 400. Ordinary
user functions, non-Lite requests and passthrough mode are not subjected to these
reserved-definition rules. Image disabling runs before validation.

No Rust dependency is required to build or run CPA. Go embeds only JSON fixtures.
