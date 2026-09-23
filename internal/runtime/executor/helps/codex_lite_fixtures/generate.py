#!/usr/bin/env python3
"""Export reserved Lite tools using the pinned CLI's actual Rust schema pipeline.

Usage: python3 generate.py /path/to/source/codex-rs /tmp/export-workdir [0.156.0|0.154.0]
Requires cargo/rustc 1.95.0. It never modifies the source checkout or calls an API.
Only schema-producing code is compiled; unrelated runtime-only types are stubbed.
All production schema types, attributes, descriptions, parsers and serializers
are copied from the source without modifying their behavior.
"""

import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tomllib

VERSION = sys.argv[3] if len(sys.argv) > 3 else "0.156.0"
CLI_COMMIT = {"0.156.0": "fe74a774532af67b5a4a3dec03ce9469e17f89af",
              "0.154.0": "6b9826e3aa83b1a5947db50f4332cb9c65f1b340"}[VERSION]
OUTPUT = Path(__file__).resolve().parent / VERSION
source = Path(sys.argv[1]).resolve()
work = Path(sys.argv[2]).resolve()
crate = work / ("exporter-" + VERSION)
crate.mkdir(parents=True, exist_ok=True)
(crate / "src").mkdir(exist_ok=True)
source_hashes = {}
expected_manifest = json.loads((OUTPUT / "provenance.json").read_text()) if (OUTPUT / "provenance.json").exists() else None


def read(name):
    data = (source / name).read_bytes()
    source_hashes[name] = hashlib.sha256(data).hexdigest()
    if expected_manifest:
        assert source_hashes[name] == expected_manifest["source_sha256"][name], "Wrong source revision: " + name
    return data.decode()


def section(text, start, end):
    return text[text.index(start):text.index(end, text.index(start))]


def write(name, text):
    path = crate / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)


lock_text = read("Cargo.lock")
lock = tomllib.loads(lock_text)


def version(name):
    versions = [p["version"] for p in lock["package"] if p["name"] == name]
    if name == "schemars":
        return "0.8.22"
    assert len(versions) == 1, (name, versions)
    return versions[0]


deps = {name: "=" + version(name) for name in (
    "schemars", "serde", "serde_json", "jsonptr", "urlencoding", "dirs", "dunce", "ts-rs"
)}
dependency_lines = []
for name, value in deps.items():
    features = {"serde": ["derive", "rc"], "serde_json": ["preserve_order"],
                "ts-rs": ["serde-json-impl", "no-serde-warnings"]}.get(name)
    if features:
        dependency_lines.append(f'{name} = {{ version = "{value}", features = {json.dumps(features)} }}')
    else:
        dependency_lines.append(f'{name} = "{value}"')
write("Cargo.toml", '[package]\nname = "cpa-codex-lite-export"\nversion = "0.0.0"\nedition = "2024"\n'
      '[dependencies]\n' + "\n".join(dependency_lines) + '\n[profile.dev]\ndebug = 0\n')
write("Cargo.lock", lock_text)
write("rust-toolchain.toml", '[toolchain]\nchannel = "1.95.0"\nprofile = "minimal"\n')
read("rust-toolchain.toml")

for name in ["json_schema.rs", "json_schema/types.rs", "json_schema/compaction.rs", "json_schema/traversal.rs"]:
    write("src/" + name, read("tools/src/" + name))
for name in ["lib.rs", "absolutize.rs"]:
    write("src/absolute_path/" + ("mod.rs" if name == "lib.rs" else name),
          read("utils/absolute-path/src/" + name))

responses = read("tools/src/responses_api.rs")
wire_types = section(responses, "#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]\npub struct FreeformTool", "pub fn dynamic_tool_to_responses_api_tool")
tool_spec = read("tools/src/tool_spec.rs")
enum = section(tool_spec, "#[derive(Debug, Clone, Serialize, PartialEq)]\n#[serde(tag = \"type\")]\npub enum ToolSpec", "impl ToolSpec")
lite_serializer = section(tool_spec, "pub fn create_tools_json_for_responses_lite", "/// Returns raw JSON")
default_namespace = re.search(r'pub const DEFAULT_FUNCTION_NAMESPACE[^;]+;', read("protocol/src/tool_name.rs"))[0]
write("src/wire.rs", "use serde::{Serialize, Deserialize};\nuse serde_json::Value;\nuse crate::json_schema::JsonSchema;\n"
      + default_namespace + "\n"
      # These are never instantiated by either namespace spec; output_schema is skipped.
      + "type ToolOutputSchema = Value;\ntype ResponsesApiWebSearchFilters = Value;\n"
      + "type ResponsesApiWebSearchUserLocation = Value;\ntype WebSearchContextSize = Value;\n"
      + wire_types + enum + lite_serializer)

image = read("ext/image-generation/src/tool.rs")
args = section(image, "#[derive(Debug, Deserialize, JsonSchema)]", "fn legacy_end_event")
image_spec = section(image, "fn imagegen_tool_spec()", "struct GeneratedImageOutput")
constants = "\n".join(re.findall(r'pub\(crate\) const IMAGE(?:_GEN_NAMESPACE|GEN_TOOL_NAME)[^;]+;', read("ext/image-generation/src/lib.rs")))
write("imagegen_description.md", read("ext/image-generation/imagegen_description.md"))
write("src/image.rs", "use schemars::{JsonSchema, r#gen::SchemaSettings};\nuse serde::Deserialize;\n"
      "use serde_json::{Map,Value};\nuse crate::absolute_path::AbsolutePathBuf;\nuse crate::wire::*;\n"
      "use crate::json_schema::parse_tool_input_schema;\n"
      'const IMAGEGEN_DESCRIPTION: &str = include_str!("../imagegen_description.md");\n'
      + constants + "\n" + args + image_spec
      + "pub fn export() -> ToolSpec { imagegen_tool_spec() }\n")

search = read("codex-api/src/search.rs")
search_types = section(search, "#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq, JsonSchema)]\npub struct SearchCommands",
                       "#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, JsonSchema)]\n#[serde(rename_all = \"snake_case\")]\npub enum ExternalWebAccessMode")
write("src/search.rs", "use serde::{Serialize,Deserialize};\nuse schemars::JsonSchema;\n" + search_types)
schema = read("ext/web-search/src/schema.rs").replace("use codex_api::SearchCommands;", "use crate::search::SearchCommands;")
write("src/schema.rs", schema)
web = read("ext/web-search/src/tool.rs")
web_spec = section(web, "    fn spec(&self) -> ToolSpec {", "    fn exposure(")
web_constants = "\n".join(re.findall(r'(?:pub\(crate\) )?const (?:WEB_NAMESPACE|RUN_TOOL_NAME|WEB_RUN_DESCRIPTION)[^;]+;', web))
write("web_run_description.md", read("ext/web-search/web_run_description.md"))
write("src/web.rs", "use crate::wire::*;\nuse crate::schema::commands_schema;\n"
      "use crate::json_schema::parse_tool_input_schema_without_compaction;\n"
      + web_constants + "\nstruct WebSearchTool;\nimpl WebSearchTool {\n" + web_spec + "}\n"
      + "pub fn export() -> ToolSpec { WebSearchTool.spec() }\n")
write("src/main.rs", '''#![allow(dead_code, unused_imports)]
mod absolute_path;
mod json_schema;
mod wire;
mod image;
mod search;
mod schema;
mod web;
fn main() {
    let dir = std::path::PathBuf::from(std::env::args().nth(1).expect("output directory"));
    std::fs::create_dir_all(&dir).unwrap();
    for (name, spec) in [("image_gen.json", image::export()), ("web.json", web::export())] {
        let tools = wire::create_tools_json_for_responses_lite(&[spec]).unwrap();
        assert_eq!(tools.len(), 1);
        std::fs::write(dir.join(name), serde_json::to_vec_pretty(&tools[0]).unwrap()).unwrap();
    }
}
''')
# Start with the official lockfile and let cargo add/prune only the isolated root.
subprocess.run(["cargo", "build", "--manifest-path", str(crate / "Cargo.toml")], cwd=crate, check=True)
export_lock = tomllib.loads((crate / "Cargo.lock").read_text())
official_packages = {(p["name"], p["version"], p.get("checksum")) for p in lock["package"]}
for package in export_lock["package"]:
    if package.get("source"):
        assert (package["name"], package["version"], package.get("checksum")) in official_packages, package
subprocess.run(["cargo", "run", "--locked", "--manifest-path", str(crate / "Cargo.toml"), "--", str(OUTPUT)], cwd=crate, check=True)
manifest = {
    "cli_tag": "rust-v" + VERSION, "cli_commit": CLI_COMMIT,
    "method": "isolated Rust export of unchanged schema-producing source sections",
    "rustc": subprocess.check_output(["rustc", "--version"], text=True).strip(),
    "source_sha256": dict(sorted(source_hashes.items())),
    "export_cargo_lock_sha256": hashlib.sha256((crate / "Cargo.lock").read_bytes()).hexdigest(),
    "fixtures_sha256": {name: hashlib.sha256((OUTPUT / name).read_bytes()).hexdigest() for name in ("image_gen.json", "web.json")},
}
(OUTPUT / "provenance.json").write_text(json.dumps(manifest, indent=2) + "\n")
shutil.copyfile(crate / "Cargo.lock", OUTPUT / "export.Cargo.lock")
print(json.dumps(manifest, indent=2))
