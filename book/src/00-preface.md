# Preface {.unnumbered}

A year ago, "using an AI coding agent" meant opening one tool in one terminal
and typing at it. Today a working day can involve three different agent
harnesses, a dozen models from five providers, several paid accounts, and
agents that run for hours on machines you never log in to. Each harness has its
own configuration format, its own idea of what a model name means, and its own
way of failing when something is misconfigured. None of them, by themselves,
knows how to give an agent a stable name, a mailbox, scoped credentials or a
place on a shared message board.

hrx is a launcher that sits in front of those harnesses. It starts Codex,
Claude Code or OpenCode against one model gateway, checks that the exact model
route you asked for is real and authorized before anything starts, and then
gets out of the way. Around that small core it adds the things you need once
agents stop being a single conversation: launch profiles, agent identities,
message delivery, short-lived Git credentials, isolated remote workers,
declarative networks of agents, a live dashboard, and a sweeper that cleans up
after all of it.

This book is a practical guide to that toolkit, and to the habits that make
agent teams work. It starts with one agent in one terminal and ends with a team
of six agents reviewing each other's work on isolated machines, measured by an
experiment you can repeat.

## Who This Book Is For

You already use AI coding agents. You know what a context window is, why one
model costs ten times another, and how it feels when an agent confidently
does the wrong thing. You are comfortable in a Unix shell and can read a TOML
file.

You will get the most from this book if one of these is true:

- You switch between harnesses or models and are tired of editing three
  configuration files to do it.
- You pay for models through a gateway such as LiteLLM and want every launch
  to go through it, with no surprise direct-to-provider traffic.
- You want to run more than one agent at a time and need them to have names,
  communicate, and leave an audit trail.
- You are running, or planning, experiments that compare agent setups and need
  launches to be reproducible.

You do not need to know Go to use hrx. You will see a little of it in later
chapters, where knowing how a check works explains why it refuses something.

## What You Need

- A macOS or Linux machine with Go 1.26 or later.
- At least one harness installed: OpenAI's Codex CLI, Anthropic's Claude Code,
  or OpenCode.
- An OpenAI-compatible model gateway that publishes a model catalog. The book
  uses LiteLLM, and [Chapter 2](#installing-hrx-and-launching-your-first-agent)
  explains exactly what hrx expects the gateway to provide.

Later parts add optional pieces: a message board for agents, a GitLab
instance, and a Proxmox cluster for remote workers. Each chapter that needs one
says so at the start, and every earlier chapter works without them.

## How This Book Is Organized

**Part I, Getting Started**, covers one agent on one machine.
[Chapter 1](#why-a-launcher) explains the problem hrx solves and the
principles that shape it. [Chapter 2](#installing-hrx-and-launching-your-first-agent)
installs hrx, connects it to a gateway, and launches your first agent.
[Chapter 3](#models-routes-and-reasoning) is about choosing a model: how hrx
turns a friendly name into an exact, authorized route, and how to read the
catalog when a model you expected is missing.

**Part II, Working Every Day**, covers launch profiles, agent identities and
communication, permissions, resuming sessions, and natural-language launches.

**Part III, Agents at Scale**, moves agents off your laptop: isolated remote
workers, declarative networks of agents, the dashboard and hub, cleaning up,
and per-agent Git credentials.

**Part IV, Using hrx Well**, is about judgment rather than mechanics. It walks
through a real experiment, six agents building and reviewing a feature under
two different communication designs, and draws out the patterns and
anti-patterns it revealed.

The appendices hold the command and configuration reference, a
troubleshooting guide, and a glossary.

## Conventions Used in This Book

`Constant width`
:   Commands, flags, file names, configuration keys, model routes and output.

`Constant width with CAPITALS`, such as `ROUTE`
:   A value you replace with your own.

`<Tab>`
:   Press the Tab key. Do not type the angle brackets.

::: tip
A tip suggests a faster or safer way to do something.
:::

::: note
A note adds background that you can skip on a first read.
:::

::: warning
A warning marks something that can cost money, leak a credential, or destroy
work.
:::

### Example names

Every example uses placeholder infrastructure. Substitute your own values:

| Placeholder | Stands for |
|---|---|
| `https://gateway.example.com/v1` | Your model gateway's OpenAI-compatible endpoint |
| `acme` | The account name your gateway uses for a provider, as in `or-acme/...` |
| `laptop/docs/agent` | An agent identity: host, role and agent name |
| `team-board` | A message board that agents share |
| `pve1`, `pve2` | Nodes in a Proxmox cluster |

Model names such as `claude-sonnet-5`, `gpt-6.1-sol` and `glm-5p3` are real
public model identities at the time of writing. Which of them you can use
depends on your gateway and your key, and that is the subject of
[Chapter 3](#models-routes-and-reasoning).

## Using the Examples

Commands that only inspect, such as `hrx models`, `hrx doctor` and any command
with `--dry-run`, never send an inference request. Commands that launch a
harness spend real money with your provider. Each chapter marks the boundary.

The command listings in this book that show hrx's own output, such as the
`--help` text, are generated from the hrx source when the book is built. If a
listing ever disagrees with your copy of hrx, your copy is newer or older than
the book; `hrx --version` tells you which.
