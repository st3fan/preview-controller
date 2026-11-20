# Objective

## Purpose

Make complete SDLC Intent changes easy to inspect in a working preview before they are merged or released.

## Problem and audience

Developers using the SDLC need a repeatable way to evaluate a complete PR stack in a realistic environment. Manually assembling, updating, and cleaning up previews is slow and can leave reviewers uncertain about which commit they are seeing. The project is a reusable, self-hosted tool for those developers, beginning with single-server installations such as a home lab.

## Desired outcomes

- Each active Intent can have a stable preview address that remains useful as approved candidates change.
- Every preview is traceable to an exact authorized commit.
- Developers can see environment health and deployment failures, and a failed replacement does not displace the last working preview.
- Preview environments can be operated and removed with modest ongoing effort.

## Direction

Provide a small controller that accepts preview candidates chosen by the SDLC workflow and manages their lifecycle for multiple projects. Keep the controller's contract with project deployment tooling narrow. Make preview status and actions understandable through a simple operator interface. Support self-hosted use beyond the original home-lab installation.

## Scope and non-goals

The project manages temporary preview environments for SDLC Intents. It does not decide when a PR stack is complete, plan Intents, rebase development branches, deploy production applications, or become a general CI/CD or multi-host orchestration platform.

## Guiding principles

- Deploy only an explicitly authorized, exact candidate commit.
- Preserve a working preview until its replacement is healthy.
- Make deployed identity, current state, and failures visible.
- Keep operation straightforward while respecting the host-level risks of running project code.
