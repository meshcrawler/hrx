# Why a Launcher? {#why-a-launcher}

Starting an AI coding agent looks trivial. You type `codex` or `claude`, the
tool opens, and you start talking. The difficulty arrives later and sideways:
the session that quietly ran on the wrong account, the model name that meant
one thing in one tool and another thing in the next, the agent that could not
be told apart from its three siblings in a log, the forgotten worker still
running, and billing, at three in the morning.

This chapter describes those problems, the small set of principles hrx uses to
solve them, and how the pieces fit together. Nothing here requires hrx to be
installed. If you are impatient, skip to
[Chapter 2](#installing-hrx-and-launching-your-first-agent) and come back when
something hrx does surprises you; the reason is probably here.

## Three Harnesses, One Bill

A *harness* is the program that wraps a model in an agent loop: it sends your
request, lets the model call tools such as a shell or a file editor, feeds the
results back, and repeats. This book covers the three most widely used
command-line harnesses:

- **Codex**, OpenAI's CLI, which speaks the OpenAI Responses API.
- **Claude Code**, Anthropic's CLI, which speaks the Anthropic Messages API.
- **OpenCode**, an open source harness that speaks the OpenAI-compatible Chat
  Completions API and works with many providers.

Each one is good at something, and many people use all three. Each also has
its own configuration file, its own credential mechanism, its own list of
models it knows about, and its own reaction to a model it does not know.

Meanwhile the models themselves increasingly arrive through a *gateway*: a
proxy such as LiteLLM that holds the real provider keys, issues you a single
key of its own, enforces budgets, and routes each request to OpenAI,
Anthropic, OpenRouter, Fireworks or wherever the model actually runs. A
gateway gives you one bill and one place to revoke access. It also adds a
layer of naming. The same model can be reachable through several *routes*,
one per provider account, each with its own price, rate limits and payer.

Put those together and the question "which model am I using?" has a
surprisingly long answer: which harness, which provider configuration inside
that harness, which gateway, which route on that gateway, which account pays
for it, and whether this harness can actually drive that model's tool calls.
Get any one of those wrong and you find out late: a harness silently falls
back to a default model, or a request goes to a provider directly instead of
through the gateway, or a tool call fails forty minutes into a task because the
route never supported the harness's API.

## What hrx Does

hrx is a single command that launches any of the three harnesses against your
gateway:

```sh
hrx codex --model gpt-6.1-sol --reasoning high
hrx claude --model claude-sonnet-5
hrx opencode --model glm-5p3
```

Before it starts anything, hrx:

1. Reads the harness's existing provider configuration to find your gateway
   and the credential you already use for it.
2. Fetches the gateway's catalog and asks the gateway which routes your key is
   allowed to call.
3. Resolves the name you gave to exactly one authorized route that this
   harness is declared compatible with, and checks the reasoning level against
   what that route advertises.
4. Writes whatever the harness needs to use that route, without touching your
   own configuration files.
5. Replaces itself with the harness process.

If any step fails, nothing launches. If every step succeeds, you are talking to
an unmodified Codex, Claude Code or OpenCode, with the route you meant.

That is the core. Everything else in this book builds on it:

- **Profiles** name a harness, route and reasoning level so a team can say
  `hrx --profile reviewer` and get the same launch every time.
- **Agent identities** give each agent a stable name of the form
  `HOST/ROLE/AGENT`, separate from any one session, so its messages, commits
  and costs can be traced back to it.
- **Communication** connects an agent to a message board and direct messages,
  and delivers new messages into the running session as turns.
- **Credentials** for Git hosting are issued per agent identity, short-lived,
  and refreshed during long sessions.
- **Remote workers** run an agent in an isolated Linux container on a Proxmox
  cluster, prove it can communicate before giving it work, and keep a durable
  record of everything needed to inspect, resume or destroy it.
- **Networks** declare a team of agents in one TOML file and launch them
  together.
- **The dashboard** shows every running agent, its model and its activity.
- **The sweeper** finds what launches left behind and, with your approval,
  backs it up and removes it.

## The Principles

hrx's behavior follows from a handful of rules. When it refuses something,
one of these is usually the reason.

### Exact routes, never substitutes

A model name is resolved to one exact gateway route or the launch fails. If
`gpt-6.1-sol` is available through two accounts and neither is the gateway's
public alias, hrx lists both and asks you to choose. It never picks a payer for
you, and it never quietly swaps in a "close enough" model.

### Fail closed

A launch revalidates authorization against the live gateway every time. A
stale cache can feed tab completion, but it can never authorize a launch. A
failed refresh keeps the last good cache and stops. Unknown capabilities, such
as a reasoning level the catalog does not declare, are treated as unavailable
rather than guessed.

### No new credentials

hrx uses the credentials you have already configured for each harness: an
environment variable, a helper command, or a private key file. It never mints
gateway keys, never writes key values to its cache, and suppresses credential
output and HTTP error bodies so a key cannot leak into a log. Features that do
need credentials, such as Git access for an agent, get them from a broker you
run, scoped to one agent identity, and short-lived.

### The harness stays native

hrx does not wrap the harness in a shell or proxy its terminal. After
validation it calls `exec`, so the harness keeps hrx's process ID, terminal,
signals and exit status. Everything after `--` on the command line goes to the
harness untouched, with one exception: options that would override the model or
provider are rejected, because they would bypass the route hrx just validated.

### Look before you leap

Almost every command has a read-only form. `--dry-run` resolves a launch and
prints exactly what would run. `hrx doctor` checks configuration, credentials,
gateway reachability and authorization without sending an inference request.
Remote launches have `--plan`. The sweeper only plans unless you name, one by
one, what it may remove.

### Records outlive sessions

An agent identity outlives any one harness session. A remote deployment keeps a
durable record of its container, lease, session and credentials so it can be
inspected, resumed or destroyed by name, even after your laptop restarts.
Recovering from a failure means rerunning the same command with the same name,
never deleting state by hand.

::: sidebar
### Why not just a shell alias?

A shell alias can save typing: `alias cs='codex -m gpt-6.1-sol'`. It cannot
tell you that your key lost access to that route yesterday, that the route
exists on two accounts, or that the harness you are about to start has never
been declared compatible with it. Each of those failures turns into a
confusing error minutes into a session, or into no error at all and a
surprising bill. A launcher is worth having precisely because it checks things
an alias cannot see.
:::

## How the Pieces Fit

Figure 1-1 shows the path of a single launch.

```text
           you
            |  hrx claude --model claude-sonnet-5
            v
   +-----------------+   catalog + /v1/models   +--------------------+
   |       hrx       | -----------------------> |   model gateway    |
   | resolve, check, | <----------------------- | (e.g. LiteLLM)     |
   | prepare, exec   |   authorized routes      +---------+----------+
   +--------+--------+                                    |
            | exec                                        | routes to
            v                                             v
   +-----------------+   API requests via gateway   provider accounts
   |  Claude Code    | ---------------------------> (Anthropic, OpenRouter,
   |  (unmodified)   |                               Fireworks, ...)
   +-----------------+
```

*Figure 1-1. One launch: hrx validates against the gateway, then becomes the harness.*

The gateway publishes two things hrx relies on. The first is the standard
OpenAI-compatible `/v1/models` endpoint, which answers "which routes may this
key call?" The second is a catalog, served at `/dashboard/models.json`, which
answers "which models exist, through which routes, and which harnesses has
each route been declared compatible with?" The catalog is what lets hrx tell
the difference between a model you cannot use and a model nobody has checked
yet. [Chapter 3](#models-routes-and-reasoning) reads it in detail.

When an agent needs more than a model, later chapters add more boxes to this
picture: a message board that delivers notifications into the session, a
credential broker that issues Git tokens, a Proxmox cluster that hosts remote
workers, and a dashboard that watches them all. The launch path stays the same.
hrx validates, prepares and then gets out of the way.

## What hrx Is Not

It helps to be clear about the edges.

- **hrx is not a harness.** It has no agent loop, no tools and no prompt of its
  own beyond optional guidance it adds for communication. The model talks to
  Codex, Claude Code or OpenCode, exactly as it would without hrx.
- **hrx is not a gateway.** It does not route, retry or fail over requests.
  Public aliases on your gateway keep their failover policy; hrx just refuses
  to guess between routes the gateway has not aliased.
- **hrx is not an orchestrator that writes your prompts.** A network file says
  which agents to start, with what models, identities and boards. What they do
  is up to the prompts you give them and the conversations they have.
- **hrx does not grant permissions.** It never widens a harness's filesystem
  or command permissions. The one permission switch it offers, `--yolo`, maps
  to each harness's own approval setting and is off unless you ask.

## Summary

Running agents well is mostly a matter of knowing, at every moment, which
model each agent is using, who is paying, what the agent is allowed to touch,
and how to stop it. hrx makes those facts explicit and checks them before a
launch rather than after a failure. The next chapter installs it and puts the
first of those checks to work.
