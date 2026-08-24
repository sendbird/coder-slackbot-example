# Sendbird Security Override Guide

This fork of [coder/coder-slackbot-example](https://github.com/coder/coder-slackbot-example)
applies Sendbird's internal-app security policy. Use it (instead of the upstream) as the
starting point for any Slack bot that lets an LLM act on Slack.

Two kinds of policy apply:

1. **Consistent policy — already enforced in code.** These hold for every Sendbird bot
   regardless of use case. Do not remove them.
2. **Intent-dependent decisions — you must decide and document.** These depend on what
   your bot is for. The checklist below tells you what to decide and what evidence the
   security review will ask for.

Every internal Slack app must pass an **Internal App Security Review** (Compliance Portal →
"Internal App Security Review" request) **before** the app is installed to the workspace.
Reviewers evaluate against the internal runbook
([Internal App Security Review — Security Review Standard](https://sendbird.atlassian.net/wiki/spaces/CPLAT/pages/4295032848),
Sendbird-internal link).

## 1. Consistent policy (enforced in this fork's code)

| Control | Where | What it does |
|---|---|---|
| Fail-closed caller allowlist | `main.go` (`SLACK_ALLOWED_USER_IDS` / `SLACK_ALLOW_ALL_USERS`) | The bot refuses to start unless you either list the Slack user IDs allowed to trigger it, or explicitly declare it open to all members. Unauthorized mentions are logged and ignored. |
| Channel pinning for model tools | `main.go` (`verifyToolCall`) | Every LLM tool call is rejected unless it targets the channel the chat was started in, and any tool not explicitly classified is denied. A prompt-injected model cannot post to, edit in, or read from other channels the bot is a member of. Pinning is channel-level; threads inside the pinned channel are not restricted. |
| No employee PII to the LLM | `main.go` (`slack_get_user_info`) | User email and admin status are not returned to the model. `users:read.email` is removed from the manifest. |

These make the upstream example's two structural risks (anyone-can-trigger, injection→
arbitrary-channel action) non-issues by default. Keep them intact when you customize.

## 2. Intent-dependent decisions (decide, then document in your review request)

### 2.1 Caller scope — who may trigger the bot?

The review question is **not** "is it limited to one person?" but "**is the actual
enforcement aligned with your declared intent?**" — and Slack structure alone is not
enforcement (anyone in the workspace can `/invite` the bot or mention it).

| Your intent | What to configure | What to state in the review |
|---|---|---|
| Personal bot (only me) | `SLACK_ALLOWED_USER_IDS` = your user ID | "Allowlist enforced in code, single user." |
| Team bot | `SLACK_ALLOWED_USER_IDS` = team member IDs (or replace with a group lookup) | Who maintains the list and how it changes. |
| Open to all employees | `SLACK_ALLOW_ALL_USERS=true` | State explicitly that org-wide invocation is intended, **and** confirm the confused-deputy invariant below. |

**Invariant that holds at any scope (confused deputy):** the bot's effective access must
not exceed the caller's own permissions. Even an intentionally open bot fails review if a
caller can use it as a proxy to reach data or actions they could not reach themselves.
Opening the caller scope also raises the injection bar: more callers = larger prompt-
injection attacker population, so channel pinning / human-approval gates become mandatory,
not optional.

### 2.2 Channel-history scopes — does the bot need to read threads?

`channels:history` and `groups:history` are broad-read scopes and are flagged in review.

- **DM-notification bots** (post results, no thread reading): remove both scopes and the
  `slack_get_thread_replies` tool; add `im:history`/`im:read` only if the bot works in DMs.
- **Thread-context bots**: keep the scopes, keep channel pinning, and justify per scope in
  the review request ("reads replies only in the invoking **channel** — channel-level
  enforcement by `verifyToolCall`; note that threads within that channel are not pinned,
  so state this residual honestly").

### 2.3 Secrets — where do the four tokens live?

`SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `CODER_URL`, `CODER_SESSION_TOKEN` are env vars in
this example. For anything beyond a local experiment:

- Store them in the approved secrets chain for your platform (e.g. Doppler → AWS Secrets
  Manager → external-secrets for K8s deployments). Never commit them or bake them into images.
- `CODER_SESSION_TOKEN` must come from a **dedicated service account**, not your personal
  Coder account, and with the narrowest role available.
- Declare each credential in the review request: owner (personal vs service account),
  privilege (read-only?), storage location, rotation plan.

### 2.4 Model blast radius — what can a successful injection do?

The LLM here has write-capable Slack tools, so review will ask: *if a prompt injection
succeeds, what is the worst case?* With this fork's defaults the answer is "post/edit/react
in the invoking channel only." If you add tools (files, external APIs, workspace exec),
re-answer that question and add a human-approval gate for anything that mutates systems
or sends data outside the invoking thread.

### 2.5 Installation gating

Do **not** click "Install to Workspace" before the security review is approved. Attach the
App ID and the full `slack-app-manifest.yaml` to the review request (manifests contain no
secrets), and state the install status explicitly.

### 2.6 Logging & retention

The example logs invocations (user, channel, text) to stderr. For production use, ship
logs somewhere durable and state the retention period in the review. Remember that
anything the bot posts remains in Slack history permanently — don't let it quote
access-restricted content.
