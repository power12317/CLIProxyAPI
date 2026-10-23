# Codex client SIWC policy

SIWC credentials use the existing `use_siwc` field. The fork additionally supports
an independent, per-credential `allow_codex_client_siwc` preference:

| `use_siwc` | `allow_codex_client_siwc` | Native Codex client | Other clients |
| --- | --- | --- | --- |
| false or absent | any | Ordinary OAuth | Ordinary OAuth |
| true | true, absent, or null | SIWC | SIWC |
| true | false | Ordinary OAuth | SIWC |

The default preserves the fork's existing SIWC behavior. Setting the new field to
false selects the official CPA policy introduced in commit `d946c4fb` for that
credential. The official Codex client detector also recognizes desktop and exec
clients. An enabled SIWC path with missing SIWC tokens fails instead of silently
switching partitions.

Use the existing `PATCH /v8/management/credentials/fields` endpoint:

```json
{
  "name": "codex-account.json",
  "use_siwc": true,
  "allow_codex_client_siwc": true
}
```

Read the preference from the full credential JSON. Sending null removes the
field and restores the default. Token refresh and ordinary status saves preserve
this preference; editing it does not remove the SIWC binding or either token
partition. WebSocket reuse checks the selected partition as well as the existing
connection fingerprint.

Codex Runtime retains its existing ordinary-OAuth execution path. This preference
does not change the Runtime switch or add automatic mode linkage.
