# Running Agent Teams with hrx: outline and status

Audience: engineers who already use AI coding agents and want to run several,
reproducibly, through one model gateway. Every example uses placeholder
infrastructure (see book/README.md).

| Status | Meaning |
|---|---|
| drafted | Written, builds, ready for review |
| planned | Scope agreed, not written |

## Front matter

| Chapter | Status | Covers |
|---|---|---|
| Preface | drafted | Audience, prerequisites, organization, conventions, placeholders |

## Part I: Getting Started

| # | Chapter | Status | Covers |
|---|---|---|---|
| 1 | Why a Launcher? | drafted | Harnesses, gateways and routes; what hrx does; principles; architecture; non-goals |
| 2 | Installing hrx and Launching Your First Agent | drafted | Gateway contract, install, completion, per-harness gateway setup, refresh, doctor, dry-run, first session, passthrough, `--yolo`, resume |
| 3 | Models, Routes and Reasoning | drafted | Identities, routes, aliases, resolution, `models`, `catalog`, statuses, evidence badges, `--latest`, reasoning, troubleshooting, freshness |

## Part II: Working Every Day

| # | Chapter | Status | Covers |
|---|---|---|---|
| 4 | Launch Profiles | planned | `[profiles.NAME]`, validation, private credential files, team conventions |
| 5 | Agent Identities and Communication | planned | `HOST/ROLE/AGENT`, message boards, targeted vs whole-board delivery, direct messages, read/handle/ACK, mail and bulletin boards |
| 6 | Sessions, Permissions and Recovery | planned | Resume and continue in depth, the private session index, supervision hooks, what survives a restart |
| 7 | Natural-Language Launches | planned | `--auto`, the confirm/reprompt loop, safety boundaries, why scripts must not use it |

## Part III: Agents at Scale

| # | Chapter | Status | Covers |
|---|---|---|---|
| 8 | Remote Workers | planned | `hrx run --backend=proxmox`, `--plan`, readiness challenge, leases, `hrx remote` lifecycle, deployment state |
| 9 | Networks of Agents | planned | `network.toml`, roles, required roles, fingerprints, `--only`, follow threads |
| 10 | Watching Agents: Dashboard and Hub | planned | Dashboard rows and sources, multi-host hub enrolment, what "quiet" means |
| 11 | Cleaning Up | planned | `hrx sweep`: plan vs apply, ownership and protective tags, backup before destroy |
| 12 | Credentials for Agents | planned | The GitLab service-account broker, identity memberships, token refresh |

## Part IV: Using hrx Well

| # | Chapter | Status | Covers |
|---|---|---|---|
| 13 | Case Study: Six Agents, One Feature | planned | The ttrac-lab experiment: broadcast vs relevance-filtered boards, stall handling, cost and context pollution, raised-but-unfixed concerns |
| 14 | Patterns and Anti-Patterns | planned | Model mix per role, review gates, stall rules, budget caps, keeping records, when not to add agents |

## Appendices

| | Appendix | Status | Covers |
|---|---|---|---|
| A | Command Reference | planned | Generated from `hrx --help` and per-command help |
| B | The Gateway Catalog Contract | planned | `/dashboard/models.json` schema, harness capability declarations, test evidence, a builder script for LiteLLM |
| C | Configuration Reference | planned | `config.toml` tables and keys |
| D | Troubleshooting | planned | Symptom → cause → fix, collected from all chapters |
| E | Glossary | planned | |

## Promises the draft already makes

Chapter 2 refers to Appendix B, including a script that builds a catalog from a
LiteLLM configuration. That script does not exist yet; see RELEASE-READINESS.md.
