# Execution Plans

Exec-plans are optional, time-bounded working notes for larger features. They are not a second architecture doc tree — keep durable decisions in [`../decisions/`](../decisions/) and enforceable rules in [`../*-guidelines.md`](../).

## When to create one

Create an exec-plan when a change spans multiple packages/PRs, needs an ordered rollout, or must track open questions before coding. Skip them for small, single-PR fixes.

## Where they live

```text
exec-plans/
└── active/              # Feature-specific plans while work is in flight
```

Use a short descriptive filename (for example `active/network-policy-egress-plan.md`). There is no required template in this repository yet; a concise checklist with goal, steps, risks, and open questions is enough. Prefer linking an OpenShift enhancement under [`../references/enhancements.md`](../references/enhancements.md) when the work needs cross-repo design review.

## How to use / complete

1. Draft the plan in `active/` and link it from the Jira issue or PR.
2. Update the plan as assumptions change; keep it short.
3. When the feature lands, delete the plan from `active/` and capture lasting decisions as an ADR in [`../decisions/`](../decisions/).
