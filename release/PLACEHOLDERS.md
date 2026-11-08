# Public placeholders

HRX is published as a cleaned export (see release/README.md). Code, tests and
public docs use these placeholders instead of real infrastructure. Use them
consistently so tests, docs and the book agree.

| Real kind | Placeholder |
|---|---|
| Model gateway host | `gateway.example.com` (`https://gateway.example.com/v1`) |
| GitLab host (HTTPS and SSH) | `gitlab.example.com`; group `acme`; project `acme/hrx` |
| GitLab broker host | `broker.example.com` |
| Dashboard host | `dashboard.example.com` (`https://dashboard.example.com/hrx`) |
| Pitput (message board) server | `pitput.example.com` |
| Other private hosts / tailnet names | `NAME.example.com` |
| Operator account / SSH user | `operator` (`operator@host.example.com`) |
| Laptop / agent host component of an identity | `laptop` (e.g. `laptop/dev/agent`) |
| Proxmox nodes | `pve1`, `pve2`, `pve3` |
| Private IPv4 addresses | `192.0.2.0/24` (TEST-NET-1), e.g. `192.0.2.10`, `.11`, `.12` |
| Primary gateway/provider account | `acme` (`or-acme/...`, `fw-acme-...`) |
| Second account | `contoso` (`tfy-contoso/...`, `fireworks-contoso`) |
| Third account | `acme-dev` (e.g. `openrouter/acme-dev`) |
| Personal source directory | `~/src` |
| Agent workspace root | `~/hrx-agents` |
| Dolt / database host | `db.example.com` |
| A person's name or email | `operator`, `operator@example.com` |

Rules:

- Prefer `.example.com`, `.example` or `.test` names; never invent a real domain.
- Keep real public model names (`claude-sonnet-5`, `gpt-6.1-sol`, `glm-5p3`) and
  real public provider names (OpenRouter, Fireworks, TrueFoundry, OpenAI,
  Anthropic).
- A test that asserted on a real name asserts on the placeholder instead; do
  not weaken the assertion.
