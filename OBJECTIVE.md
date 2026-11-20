# Objective

## Vision

Developers and AI agents work together in a tight feedback loop, so an idea can become something you can try while it’s still fresh.

## Mission

Give agents the ability to get changes running, show what happened, and help diagnose failures. Give developers a quick way to try the result, steer the next iteration, and decide when it’s ready.

Shift routine preview work to agents so developers can focus on deciding what to build and whether it does what they intended.

## Problem and audience

Developers working with AI agents need to experience a change to judge whether it meets their intent. Manually preparing previews, finding the right revision, and investigating failures interrupts that feedback loop.

Preview Controller is for developers using SDLC Intents and the agents helping them, with temporary previews on infrastructure they control.

## Desired outcomes

- Developers can move quickly from an idea or change to a working preview they can try and discuss.
- Agents can help prepare and update previews, report what happened, and provide evidence to diagnose failures.
- Everyone can identify the exact revision being tried and distinguish a working preview from a pending or failed attempt.
- Each Intent has a stable preview address as authorized candidates change, and a failed replacement preserves the last working preview.
- Keeping previews available and removing them when they are no longer needed takes little routine effort.

## Direction

Make the path from an authorized change to useful feedback straightforward for both developers and agents. Keep preview actions, outcomes, and failures understandable so the next iteration can follow from what people observe.

Provide a small, reusable controller for temporary previews across projects, supporting self-hosted use beyond the original home lab. Keep its contract with project deployment tooling narrow. These are enduring goals; current capabilities and setup are documented in README.md, and technology choices belong in TECHNOLOGY.md.

## Scope and non-goals

The project manages temporary, self-hosted preview environments for SDLC Intents. Developers decide what to build and when a result is ready.

It does not plan Intents, decide when a PR stack is complete, rebase development branches, automatically merge or release changes, or deploy to production. It is not a general CI/CD system or a general-purpose or multi-host orchestration platform.

## Guiding principles

- Put people and the outcomes they need ahead of implementation details.
- Run only explicitly authorized, exact candidate revisions; agent assistance does not expand that authorization.
- Make the running revision, current state, and failures observable.
- Preserve a working preview until its replacement is healthy.
- Keep operation straightforward while respecting the host-level risks of running project code.
