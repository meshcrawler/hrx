# Validated launch profiles

A profile selects a harness, an exact LiteLLM gateway route, optional reasoning,
and a reference to a private JSON credential file. It does not select an agent
identity, directory, board, GitLab account or permission policy.

Add profiles to `~/.config/hrx/config.toml` (or the file selected by
`HRX_CONFIG`). Gateway URLs and Codex templates continue to come from the existing
harness/provider configuration; profiles do not redefine them.

```toml
[profiles.contoso-sol-low]
harness = "codex"
model = "codex-contoso/gpt-6.1-sol"
reasoning = "low"
credential_file = "~/.config/litellm/contoso-only.json"
```

```sh
hrx profiles list
hrx profiles show contoso-sol-low --json
hrx profiles validate contoso-sol-low --offline
hrx profiles validate --all --json
hrx --profile contoso-sol-low --dry-run
hrx --profile contoso-sol-low --gitlab \
  --agent-identity laptop/hrx-dev/agent --pitput --pitput-board hrx-dev
```

The example assumes the gateway declares and authorizes that exact route for
your key. It does not grant access. LiteLLM decides which upstream backs its
alias; an alias name is not independent proof of payer accounting.

## Precedence and credentials

Explicit `--model`, `--reasoning` and `--credential-file` override the profile.
The resulting combination is revalidated, and dry-run output includes provenance.
`hrx codex --profile contoso-sol-low` is also supported. An explicitly conflicting
harness is an error. Put HRX flags before `--`; they are never forwarded as native
Codex, Claude or OpenCode profile options.

The four table fields shown above are the complete schema. `reasoning` may be
omitted; the gateway's declared default is then used (or no effort for models
without effort controls). Unknown fields and non-string values are errors.
Model strings must match an exact gateway route online. A bare name that is only
a canonical identity is rejected with available candidate routes; no first-route
or payer fallback occurs. Exact routes without a slash are supported when the
gateway actually declares that string as a route.

Credential files contain `{"key":"..."}`. They must be regular files owned by
the current user, at most 64 KiB, with no group/other permissions (normally 0600).
The final path component cannot be a symlink; parent directory symlinks are
allowed. `~/` expands to the user's home, and other relative references are
resolved relative to the HRX configuration file. Contents are read through a
bounded file descriptor and are never shown in diagnostics or command arguments.
Provider environment keys and auth helpers are replaced for this launch only;
the parent process environment is not changed. Codex receives the key through
`HRX_PROFILE_GATEWAY_KEY`, OpenCode through `HRX_GATEWAY_KEY`, and Claude through
its existing gateway header mechanism.

## Validation

`show` reads only the configuration, not credentials. `validate --offline` checks
the schema and credential file without network requests, enrollment, native
sessions or bridges. It explicitly reports that authorization was not attempted.
Offline validation cannot establish whether a model string names an exact route.

Default `validate` fetches a fresh catalog and key-scoped `/v1/models` authorization,
then checks exact route, harness capability, reasoning, input/output limits, and
Codex template availability. It uses the same checks as profile launches. Each
profile is checked independently, including distinct keys on the same gateway;
no cached authorization is treated as current. Catalog cache files may be written.
Gateway outages, stale catalogs, HTTP authorization failures, missing routes,
undeclared compatibility and unknown capacities fail validation. No inference,
GitLab enrollment, Pitput startup or session mutation occurs.

`validate --all` reports every profile, even if an earlier one fails. JSON output
is an array of per-profile results. Exit status is 0 when all requested checks
pass and 1 for invalid arguments, configuration or validation failure. Successful
validation establishes declared compatibility, not native live proof, provider
uptime or successful billing. Profile `--dry-run` performs online validation and
prints the effective launch without starting an agent.

## Resume

Private session metadata stores the effective profile name, nonsecret configuration
fingerprint, harness, exact route, reasoning and credential reference. The fingerprint
never includes key bytes. `hrx codex --resume SESSION_ID` restores this snapshot,
even if the original profile is later edited or deleted. Rotating the JSON key at
the same reference is supported; resume performs fresh authorization with the new
key. Explicit `--profile` or CLI overrides select a deliberate new configuration.
Old sessions without a snapshot retain the existing legacy resume requirements.
Permission policy, identity and board ownership rules remain independent.

Credential references are private resume metadata and dry-run output; they are
not added to the shared dashboard or hub payload.

## Other accounts and harnesses

To create a personal gateway Codex profile, copy the four-field table, choose a
new name, and fill in the exact route and private key file authorized for your
personal account. This feature does not turn a direct ChatGPT subscription into
a LiteLLM credential; existing gateway provider configuration is required.

Claude and OpenCode use the same schema with `harness = "claude"` or
`harness = "opencode"`. Select exact routes from your key's gateway catalog;
omit `reasoning` when that route/harness has no declared effort controls. Validate
before launching:

```sh
hrx profiles validate my-claude --json
hrx --profile my-claude -- --print 'Explain the current project'
hrx profiles validate my-opencode --json
hrx --profile my-opencode -- run 'Explain the current project'
```

These commands require profiles you configured with actual authorized routes.
No model, credential or access entitlement is invented by HRX.
