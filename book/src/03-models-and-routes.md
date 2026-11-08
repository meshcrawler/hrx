# Models, Routes and Reasoning {#models-routes-and-reasoning}

Choosing a model used to mean picking a name from a short list. Behind a
gateway it means three decisions at once: which model, which route to it (and
so which account pays), and how hard it should think. This chapter shows how
hrx turns the name you type into one exact route, how to read the catalog
when you want to know what is available, and how to work out why a model you
expected is missing.

Everything in this chapter is read-only. None of it sends an inference
request.

## Identities, Routes and Aliases

Three words carry most of the weight in this chapter.

A **model identity** is the model itself, independent of who serves it:
`gpt-6.1-sol`, `claude-sonnet-5`, `glm-5p3`. The catalog groups everything by
identity.

A **route** is one exact name the gateway accepts in a request, bound to one
provider account. The same identity can have several routes:

| Route | Served by |
|---|---|
| `gpt-6.1-sol` | The gateway's public alias, with its own failover policy |
| `tfy-acme/gpt-6.1-sol` | A TrueFoundry account named `acme` |
| `or-acme/openai/gpt-6.1-sol` | An OpenRouter account named `acme` |

Routes differ in price, rate limits, context limits and, importantly, in which
harnesses can drive them. A route that works with Codex's Responses API may not
work with OpenCode's Chat Completions client, and vice versa.

A **public alias** is a route the gateway operator has deliberately published
under the model's plain name. The gateway may fail over between providers
behind an alias. hrx keeps aliases intact: if you ask for `gpt-6.1-sol` and the
gateway publishes that alias, that is what the harness receives, and the
gateway's failover keeps working.

### How a name is resolved

When you pass `--model NAME`, hrx looks for, in order:

1. An exact route called `NAME` that your key may call and this harness is
   declared compatible with. If it exists, that is the answer.
2. A model identity called `NAME` with exactly one such route, or a public
   alias. That route is the answer.
3. Otherwise, the launch fails.

There is no step 4. If an identity has two usable routes and no public alias,
hrx does not pick one; it prints both and asks you to choose:

```sh
hrx codex --model tfy-acme/gpt-6.1-sol --reasoning high --dry-run
```

The same applies to names that are close but not equal. hrx never treats
`gpt-6.1` as `gpt-6.1-sol` or substitutes a newer model for a retired one.

::: sidebar
### Why ambiguity is an error

Two routes to the same model can differ by an order of magnitude in price, or
bill two different budgets: a personal account and a team account, say. A tool
that chooses silently will eventually choose the expensive one for a
six-hour agent run. Making you type the route once costs a few characters;
the alternative costs a surprise on the invoice.
:::

## Listing What You Can Use

`hrx models` lists what a harness can launch right now, one line per choice:

```sh
hrx models
hrx models --harness claude
hrx models --harness opencode --latest
hrx models --json
```

It reads the local cache, so it is instant and works offline. Add `--refresh`
to update the cache first. If the cache is older than 30 minutes the command
refuses, rather than showing choices that may no longer be valid; `--stale`
shows them anyway, clearly marked, which is useful during a gateway outage
when you want to know what *was* available. A stale listing never authorizes a
launch.

`hrx models` answers "what can I start?" When you want to know why something
is *not* on that list, you need the catalog.

## Reading the Catalog

`hrx catalog` shows every model the gateway discovered, including the ones you
cannot use, together with the reason:

```sh
hrx catalog --refresh --compact
hrx catalog --compact --latest --search gpt-
hrx catalog --harness codex --search glm
hrx catalog --details --harness opencode --search deepseek
hrx catalog --output model-catalog.json
```

The catalog has three text views and one data view.

**Compact** (`--compact`) prints one row per model identity, with the provider
accounts that serve it and, for each harness, the accounts with at least one
ready route:

```text
MODEL        SOURCES          codex    claude  opencode  NEWER FAMILY MODEL
example-sol  OR-A,FW-A,TF-A   TF-A[D]  -       OR-A[D]   -
```

Account names are abbreviated (here `OR-A` might be OpenRouter/acme), and a
legend under the table explains only the abbreviations that appear. A `-`
means this harness has no ready route to the model.

**Route level** (the default) prints one row per route, with each harness's
status for that route.

**Details** (`--details`) adds everything hrx knows about each route: the
upstream model, the wire API, per-harness authorization and compatibility,
reasoning choices and defaults, context and output limits, and test dates.

**JSON** (`--json` or `--output FILE`) exports the full underlying evidence,
whatever text view you chose. The export contains no credentials, so you can
attach it to a bug report or keep it as a record of what was available on a
given day.

`--search TEXT` matches model names, routes and account names. Searching for
`codex` therefore finds models with "codex" in their name; it does *not* mean
"models usable in Codex." For that, use `--harness codex`, or `hrx models`.

### Route statuses

Every route has a status for each harness. These five words explain almost
every "where is my model?" question:

| Status | Meaning | What to do |
|---|---|---|
| `ready` | Your key may call it and the gateway declares it compatible with this harness | Launch it |
| `not-authorized` | The route exists, but your key may not call it | Ask the gateway operator; refreshing cannot widen a key |
| `compatibility-unverified` | Your key may call it, but nobody has declared it compatible with this harness | Use another harness, or get the route tested and declared |
| `no-gateway-route` | A provider offers the model, but the gateway has no route for it | Ask the operator to add a route |
| `unavailable` | The catalog marks the model or its source unavailable | Wait, or choose another route |

The difference between `not-authorized` and `compatibility-unverified` matters.
A key expansion can move a route from the first to the second without making it
launchable. Authorization says you may *pay* for the route; compatibility says
the harness can *use* it.

::: note
Gateways often grant access with wildcards, such as `tfy-acme/*`. hrx honors
those only for concrete routes the catalog has discovered under that account.
A wildcard never grants access to another account, or to a model nobody has
discovered.
:::

### Evidence badges

Declaring a route compatible is a claim. Testing it is evidence. The catalog
can record when a route was last live-tested with each harness, and hrx shows
that as a badge next to the account:

| Badge | Meaning |
|---|---|
| `D` | Declared compatible; no live test recorded |
| `V` | Live-tested within the last 7 days |
| `S` | Live-tested, but more than 7 days ago |
| `U` | A test is claimed, but its date is missing, invalid or in the future |
| `cached-` | Prefix shown when the listing comes from stale cache |

The 7-day window is a display policy, not a rule: a stale test does not make a
route unlaunchable. hrx never invents test dates, and refreshing authorization
does not refresh them. Only the catalog's own dated, per-harness evidence can
produce a `V`.

## Keeping Up with New Models: `--latest`

Providers release new generations often enough that a full list fills up with
superseded models. `--latest` hides older numeric generations within the same
family and variant:

```sh
hrx catalog --latest --search gpt-
hrx models --harness opencode --latest
hrx opencode --latest --model <Tab>
```

With `--latest`, `gpt-6.1-sol` hides `gpt-6-sol`, but not a different variant
such as `gpt-6.1-terra`. Versions compare numerically, so `5.10` is newer than
`5.9`. Names hrx does not recognize, dated snapshots and distinct suffixes stay
visible.

Two safeguards keep this from hiding something you need. First, an older
generation stays visible if it is the newest one *this harness* can actually
use; a successor you cannot launch never hides your only working choice. Second,
`--latest` only changes what is listed. An explicit `--model` for an older
generation still launches.

The `NEWER FAMILY MODEL` column tells you when a newer generation exists, even
if you cannot use it yet. That is a hint, not a recommendation: newer is not
always better for a given task, and it is not always cheaper.

## Reasoning Levels

Many models accept a reasoning effort, from `low` to `xhigh` or `max`, that
trades latency and cost for more deliberate answers. Which levels exist depends
on the model *and* the harness, so hrx reads them from the catalog rather than
assuming:

```sh
hrx codex --model gpt-6.1-sol --reasoning high
hrx codex --model gpt-6.1-sol --reasoning <Tab>
```

The reasoning picker follows the model you selected and marks the catalog's
declared default. If you leave `--reasoning` off, the harness's normal default
applies, as declared by the catalog. If you ask for a level the route does not
advertise, the launch fails; hrx does not round to the nearest level or drop
the flag silently.

At the time of writing, reasoning selection is advertised for Codex routes.
The Claude Code and OpenCode adapters launch with the provider's default and
reject `--reasoning` with a clear error rather than ignoring it.

::: tip
Higher reasoning is not free and not always better. For agent work, a common
pattern is a fast, low-effort model for routine roles and a high-effort one for
review and planning. Chapter 4 shows how to name those
combinations once and reuse them.
:::

## Choosing Interactively

Tab completion offers the same choices as `hrx models`, filtered for the
harness on the command line:

```text
hrx codex --model <Tab>
hrx codex --model glm-5p3 --reasoning <Tab>
hrx claude --model <Tab>
hrx opencode --latest --model <Tab>
```

With fzf-tab, each candidate shows its authorization, compatibility evidence,
test date and any newer family model. The descriptions are for display only:
choosing an entry inserts just the route or the level.

Completion reads only the local cache. It never runs your credential helper,
opens a network connection or contacts the gateway, so it is fast and safe to
press repeatedly. An empty menu means the cache is missing or older than 30
minutes; refresh the harness you are completing for.

## When a Model Is Missing

Here is a short procedure for the most common question.

1. **Refresh the right harness.** Each harness has its own cache:
   `hrx refresh --harness opencode`.
2. **Check the launchable list.** `hrx models --harness opencode`. If the model
   is there, use the exact name shown.
3. **Find out why not.** `hrx catalog --refresh --harness opencode --search NAME`
   and read the status column.
4. **Ask the doctor about that exact choice.**
   `hrx doctor --harness opencode --model NAME`.

| Symptom | Likely cause |
|---|---|
| Model absent from `hrx catalog` | The gateway never discovered it; check the gateway, not hrx |
| `not-authorized` | Your key's scope; only the gateway operator can widen it |
| `compatibility-unverified` | Not declared for this harness; try another harness |
| "ambiguous" error | Several usable routes; pass the exact route |
| Empty Tab menu | Stale or missing cache; `hrx refresh --harness NAME` |
| Unsupported reasoning level | Choose one the route advertises (`--reasoning <Tab>`) |
| HTTP 429 after launch | A gateway or provider limit; `ready` does not promise capacity |

## How Fresh Is Fresh?

The rules for staleness are simple and strict:

- The cache is fresh for 30 minutes, counting the age of the gateway's own
  catalog snapshot.
- Completion shows nothing from a stale cache; `hrx models` and `hrx catalog`
  refuse unless you pass `--stale`.
- Every launch revalidates against the live gateway. There is no option to
  launch from stale data.
- A failed refresh keeps the last good cache, so a gateway hiccup cannot leave
  you with an empty or corrupt one.

The effect is that the listing you look at may be slightly old, but the launch
you start never is.

## Summary

A model name in hrx resolves to exactly one authorized, harness-compatible
route, or the launch fails and tells you why. `hrx models` shows what you can
launch, `hrx catalog` shows everything and explains the gaps, and `--latest`
keeps the list short without hiding your only working option. Reasoning levels
come from the catalog and are checked, never guessed.

You now have everything you need to launch one agent deliberately. Part II
turns those deliberate choices into reusable profiles, gives agents names,
and connects them to each other.
