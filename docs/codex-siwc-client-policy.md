# SIWC request routing

CPA direct requests select the credential's SIWC partition when `use_siwc` is
enabled, regardless of the incoming client's User-Agent. Disabling `use_siwc`
selects the ordinary credential partition. An enabled SIWC path with missing
SIWC tokens fails instead of silently switching partitions.

Codex Runtime always uses ordinary Codex OAuth credentials. OpenAI image API
requests also use the ordinary OAuth partition according to the existing image
routing policy. These request-local decisions preserve the saved SIWC preference.

Older credential files can contain `allow_codex_client_siwc`. This obsolete
field has no effect on request routing.
