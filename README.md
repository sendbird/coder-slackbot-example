# Coder Slackbot Example

> **Sendbird fork.** This fork applies Sendbird's internal-app security policy on top of
> the upstream example. Before reusing it, read **[sendbird_override.md](./sendbird_override.md)**:
> it lists the controls already enforced in code (fail-closed caller allowlist, channel
> pinning for model tools, no email exposure to the LLM) and the intent-dependent
> decisions you must make (caller scope, channel-history scopes, secrets storage)
> before submitting the app for security review.

This is a **minimal example** of a Go Slackbot that uses the [Coder chat API](https://coder.com/docs)'s **dynamic tools** feature to let an LLM interact with Slack. It is intended as a starting point — fork it, customize the system prompt, and add your own tools.

## How It Works

1. Bot is @mentioned in Slack
2. Creates a new Coder chat with dynamic tools (`slack_send_message`, `slack_react_to_message`, etc.)
3. When the LLM calls a dynamic tool → executes it against the Slack API → submits results back
4. Chat resumes until complete

Follow-up mentions in the same thread reuse the same Coder chat.

## Dynamic Tools

| Tool | Description |
|------|-------------|
| `slack_send_message` | Post a message |
| `slack_edit_message` | Edit a previously sent message |
| `slack_react_to_message` | Add or remove an emoji reaction |
| `slack_get_thread_replies` | Read thread context |
| `slack_get_user_info` | Look up a Slack user |
| `slack_report_status` | Update the typing indicator |

## Running

```bash
export SLACK_BOT_TOKEN="xoxb-..."        # Bot User OAuth Token
export SLACK_APP_TOKEN="xapp-..."        # App-Level Token (socket mode)
export CODER_URL="https://your-coder.example.com"
export CODER_SESSION_TOKEN="..."         # coder tokens create

# Caller authorization (Sendbird fork, fail-closed): one of the two is required.
export SLACK_ALLOWED_USER_IDS="U012ABCDEF,U034GHIJKL"  # who may trigger the bot
# export SLACK_ALLOW_ALL_USERS=true                    # or intentionally open to all

go build -o slackbot .
./slackbot
```

## Slack App Setup

1. Create a Slack app at https://api.slack.com/apps (or use the manifest in `slack-app-manifest.yaml`)
2. Enable **Socket Mode** (generates the `xapp-` token)
3. Subscribe to bot events: `app_mention`
4. Add bot scopes: `app_mentions:read`, `chat:write`, `reactions:write`, `users:read`, `assistant:write` — plus `channels:history`/`groups:history` only if thread reading is needed (see [sendbird_override.md](./sendbird_override.md))
5. Install to workspace

## Requirements

- **Coder**: commit [`76d89f59af42`](https://github.com/coder/coder/commit/76d89f59af42) or later (the first commit with the `WatchChats`, `UnsafeDynamicTools`, and experimental chat APIs this example depends on)
- Go 1.24+
