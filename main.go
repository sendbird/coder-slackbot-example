package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const systemPrompt = `You are a helpful Slack assistant powered by Coder.

Use the tools available to you to respond to users in Slack.

<behavior>
Do EXACTLY what the user asked, never more, never less.
Be concise, direct, and to the point.
</behavior>

<slack-status>
You MUST use slack_report_status and slack_send_message to communicate.
You have NO other way to talk to the user — they cannot see your reasoning.
Every response MUST go through slack_send_message.

BEFORE calling any other tool, ALWAYS call slack_report_status first to
show the user what you are doing.
</slack-status>

<formatting-rules>
SLACK FORMATTING RULES:
- *text* = bold
- _text_ = italics
- ~text~ = strikethrough
- <http://example.com|link text> = links
- Tables must be in a code block.
- User mentions must be in the format <@user_id>.
- Messages are limited to 3000 characters; if your response is longer,
  send follow-up messages.

NEVER USE:
- Headings (#, ##, ###, etc.)
- Double asterisks (**text**) — Slack does not support this
- Standard markdown bold/italic conventions
</formatting-rules>
`

const maxMessageLen = 3000

// Tool argument types.

type sendMessageArgs struct {
	Channel  string `json:"channel" jsonschema:"required,description=The Slack channel ID to post to"`
	ThreadTs string `json:"thread_ts,omitempty" jsonschema:"description=The thread timestamp to reply in. Always include this to keep responses in-thread."`
	Text     string `json:"text" jsonschema:"required,description=The message text in Slack mrkdwn format."`
}

type editMessageArgs struct {
	Channel string `json:"channel" jsonschema:"required,description=The Slack channel ID"`
	Ts      string `json:"ts" jsonschema:"required,description=The timestamp of the message to edit"`
	Text    string `json:"text" jsonschema:"required,description=The new message text in Slack mrkdwn."`
}

type reactArgs struct {
	Channel  string `json:"channel" jsonschema:"required,description=The Slack channel ID"`
	Ts       string `json:"ts" jsonschema:"required,description=The message timestamp to react to"`
	Reaction string `json:"reaction" jsonschema:"required,description=Emoji name without colons"`
	Remove   bool   `json:"remove_reaction,omitempty" jsonschema:"description=Set to true to remove the reaction instead of adding it"`
}

type getRepliesArgs struct {
	Channel  string `json:"channel" jsonschema:"required,description=The Slack channel ID"`
	ThreadTs string `json:"thread_ts" jsonschema:"required,description=The thread timestamp"`
}

type getUserArgs struct {
	UserID string `json:"user_id" jsonschema:"required,description=The Slack user ID"`
}

type reportStatusArgs struct {
	Channel  string   `json:"channel" jsonschema:"required,description=The Slack channel ID"`
	ThreadTs string   `json:"thread_ts" jsonschema:"required,description=The thread timestamp"`
	Status   string   `json:"status" jsonschema:"required,description=Short status message"`
	Loading  []string `json:"loading_messages,omitempty" jsonschema:"description=Messages Slack will cycle through while you work"`
}

func (b *bot) dynamicTools() []codersdk.DynamicTool {
	return []codersdk.DynamicTool{
		codersdk.NewDynamicTool("slack_send_message",
			"Send a message to Slack.",
			func(ctx context.Context, args sendMessageArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				formatted, truncated, originalLen := formatMessage(args.Text)

				blocks := []slack.Block{
					slack.NewSectionBlock(
						slack.NewTextBlockObject(slack.MarkdownType, formatted, false, false),
						nil, nil,
					),
				}
				opts := []slack.MsgOption{
					slack.MsgOptionText(args.Text, false),
					slack.MsgOptionBlocks(blocks...),
				}
				if args.ThreadTs != "" {
					opts = append(opts, slack.MsgOptionTS(args.ThreadTs))
				}

				_, msgTs, err := b.slack.PostMessageContext(ctx, args.Channel, opts...)
				if err != nil {
					return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
				}

				b.setStatus(ctx, args.Channel, args.ThreadTs, "")

				result := map[string]any{"ok": true, "ts": msgTs}
				if truncated {
					result["truncated"] = true
					result["original_length"] = originalLen
					result["note"] = fmt.Sprintf(
						"Your message was truncated from %d to %d characters. Send a follow-up message to continue.", originalLen, maxMessageLen)
				}
				return jsonResponse(result)
			},
		),
		codersdk.NewDynamicTool("slack_edit_message",
			"Edit a previously sent Slack message.",
			func(ctx context.Context, args editMessageArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				formatted, _, _ := formatMessage(args.Text)
				blocks := []slack.Block{
					slack.NewSectionBlock(
						slack.NewTextBlockObject(slack.MarkdownType, formatted, false, false),
						nil, nil,
					),
				}
				_, _, _, err := b.slack.UpdateMessageContext(ctx, args.Channel, args.Ts,
					slack.MsgOptionText(args.Text, false),
					slack.MsgOptionBlocks(blocks...),
				)
				if err != nil {
					return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
				}
				return codersdk.DynamicToolResponse{Content: `{"ok":true}`}, nil
			},
		),
		codersdk.NewDynamicTool("slack_react_to_message",
			"Add or remove an emoji reaction on a Slack message.",
			func(ctx context.Context, args reactArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				name := strings.Trim(args.Reaction, ":")
				ref := slack.ItemRef{Channel: args.Channel, Timestamp: args.Ts}
				if args.Remove {
					if err := b.slack.RemoveReactionContext(ctx, name, ref); err != nil {
						return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
					}
				} else {
					if err := b.slack.AddReactionContext(ctx, name, ref); err != nil {
						return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
					}
				}
				return codersdk.DynamicToolResponse{Content: `{"ok":true}`}, nil
			},
		),
		codersdk.NewDynamicTool("slack_get_thread_replies",
			"Read all replies in a Slack thread for additional context.",
			func(ctx context.Context, args getRepliesArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				msgs, _, _, err := b.slack.GetConversationRepliesContext(ctx,
					&slack.GetConversationRepliesParameters{
						ChannelID: args.Channel,
						Timestamp: args.ThreadTs,
					})
				if err != nil {
					return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
				}
				type reply struct {
					User string `json:"user"`
					Text string `json:"text"`
					Ts   string `json:"ts"`
				}
				replies := make([]reply, 0, len(msgs))
				for _, m := range msgs {
					replies = append(replies, reply{User: m.User, Text: m.Text, Ts: m.Timestamp})
				}
				return jsonResponse(replies)
			},
		),
		codersdk.NewDynamicTool("slack_get_user_info",
			"Get profile information about a Slack user by their ID.",
			func(ctx context.Context, args getUserArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				user, err := b.slack.GetUserInfoContext(ctx, args.UserID)
				if err != nil {
					return codersdk.DynamicToolResponse{Content: fmt.Sprintf("slack error: %v", err), IsError: true}, nil
				}
				// Email and admin status are deliberately not exposed: user
				// email is employee PII and must not enter LLM context
				// (see sendbird_override.md).
				return jsonResponse(struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					RealName string `json:"real_name"`
					IsBot    bool   `json:"is_bot"`
					TZ       string `json:"tz"`
				}{
					ID: user.ID, Name: user.Name, RealName: user.RealName,
					IsBot: user.IsBot, TZ: user.TZ,
				})
			},
		),
		codersdk.NewDynamicTool("slack_report_status",
			"Update your visible status in the Slack thread. Call this BEFORE any other tools.",
			func(ctx context.Context, args reportStatusArgs, _ codersdk.DynamicToolCall) (codersdk.DynamicToolResponse, error) {
				b.setStatus(ctx, args.Channel, args.ThreadTs, args.Status, args.Loading...)
				return codersdk.DynamicToolResponse{Content: `{"ok":true}`}, nil
			},
		),
	}
}

var (
	reMention      = regexp.MustCompile(`<@([UW][A-Z0-9]+)>`)
	reCodeBlock    = regexp.MustCompile("(?s)```.*?```")
	reInlineCode   = regexp.MustCompile("`[^`]+`")
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reBold         = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reHeading      = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reCodeLang     = regexp.MustCompile("(?m)^```[a-zA-Z]+\\s*$")
)

type bot struct {
	logger    *slog.Logger
	slack     *slack.Client
	coder     *codersdk.ExperimentalClient
	botUID    string
	tools     []codersdk.DynamicTool
	toolMap   map[string]codersdk.DynamicTool
	userCache *userCache
	// allowedUsers is a fail-closed caller allowlist; allowAll must be set
	// explicitly to open the bot to every workspace member
	// (see sendbird_override.md).
	allowedUsers map[string]bool
	allowAll     bool
}

type userCache struct {
	mu      sync.RWMutex
	entries map[string]userCacheEntry
	ttl     time.Duration
}

type userCacheEntry struct {
	user      *slack.User
	fetchedAt time.Time
}

func newUserCache(ttl time.Duration) *userCache {
	return &userCache{
		entries: make(map[string]userCacheEntry),
		ttl:     ttl,
	}
}

func (c *userCache) get(id string) (*slack.User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[id]
	if !ok || time.Since(e.fetchedAt) > c.ttl {
		return nil, false
	}
	return e.user, true
}

func (c *userCache) set(id string, user *slack.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = userCacheEntry{user: user, fetchedAt: time.Now()}
}

func (b *bot) lookupUser(ctx context.Context, id string) (*slack.User, error) {
	if u, ok := b.userCache.get(id); ok {
		return u, nil
	}
	u, err := b.slack.GetUserInfoContext(ctx, id)
	if err != nil {
		return nil, err
	}
	b.userCache.set(id, u)
	return u, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mustEnv := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		logger.Error("required env var not set", "key", key)
		os.Exit(1)
		return ""
	}

	u, err := url.Parse(mustEnv("CODER_URL"))
	if err != nil {
		logger.Error("invalid CODER_URL", "error", err)
		os.Exit(1)
	}
	base := codersdk.New(u)
	base.SetSessionToken(mustEnv("CODER_SESSION_TOKEN"))

	api := slack.New(mustEnv("SLACK_BOT_TOKEN"), slack.OptionAppLevelToken(mustEnv("SLACK_APP_TOKEN")))
	sm := socketmode.New(api, socketmode.OptionDebug(false))

	auth, err := api.AuthTestContext(ctx)
	if err != nil {
		logger.Error("slack auth failed", "error", err)
		os.Exit(1)
	}
	logger.Info("connected to Slack", "bot_user_id", auth.UserID, "team", auth.Team)

	allowAll := os.Getenv("SLACK_ALLOW_ALL_USERS") == "true"
	allowedUsers := make(map[string]bool)
	for _, id := range strings.Split(os.Getenv("SLACK_ALLOWED_USER_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			allowedUsers[id] = true
		}
	}
	if !allowAll && len(allowedUsers) == 0 {
		logger.Error("caller authorization not configured: set SLACK_ALLOWED_USER_IDS to a comma-separated list of Slack user IDs, or set SLACK_ALLOW_ALL_USERS=true to intentionally open the bot to all workspace members (see sendbird_override.md)")
		os.Exit(1)
	}

	b := &bot{
		logger:       logger,
		slack:        api,
		coder:        codersdk.NewExperimentalClient(base),
		botUID:       auth.UserID,
		userCache:    newUserCache(5 * time.Minute),
		allowedUsers: allowedUsers,
		allowAll:     allowAll,
	}
	b.tools = b.dynamicTools()
	b.toolMap = make(map[string]codersdk.DynamicTool, len(b.tools))
	for _, t := range b.tools {
		b.toolMap[t.Name] = t
	}

	go b.watchChats(ctx)

	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				sm.Ack(*evt.Request)
				inner, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				if ev, ok := inner.InnerEvent.Data.(*slackevents.AppMentionEvent); ok {
					go b.handleMention(ctx, ev)
				}
			case socketmode.EventTypeConnecting:
				logger.Info("connecting to Slack socket mode...")
			case socketmode.EventTypeConnected:
				logger.Info("socket mode connected")
			case socketmode.EventTypeConnectionError:
				logger.Error("socket mode connection error", "data", fmt.Sprintf("%v", evt.Data))
			}
		}
	}()

	logger.Info("starting socket mode listener")
	if err := sm.RunContext(ctx); err != nil && err != context.Canceled {
		logger.Error("socket mode exited", "error", err)
		os.Exit(1)
	}
}

func (b *bot) handleMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	if !b.allowAll && !b.allowedUsers[ev.User] {
		b.logger.Warn("ignoring mention from unauthorized user", "user", ev.User, "channel", ev.Channel)
		return
	}
	threadTs := ev.ThreadTimeStamp
	if threadTs == "" {
		threadTs = ev.TimeStamp
	}
	logger := b.logger.With("channel", ev.Channel, "thread_ts", threadTs, "user", ev.User)

	text := strings.TrimSpace(strings.ReplaceAll(ev.Text, fmt.Sprintf("<@%s>", b.botUID), ""))
	if text == "" {
		text = "Hello!"
	}
	logger.Info("handling mention", "text", text)

	b.setStatus(ctx, ev.Channel, threadTs, "Thinking...", "Thinking...", "Working on it...", "Still working...")

	var (
		wg      sync.WaitGroup
		chatID  uuid.UUID
		chatErr error
		msg     string
	)
	key := ev.Channel + ":" + threadTs

	wg.Add(2)
	go func() {
		defer wg.Done()
		chatID, chatErr = b.findChat(ctx, key)
	}()
	go func() {
		defer wg.Done()
		msg = b.buildMessage(ctx, ev, text, threadTs)
	}()
	wg.Wait()

	if chatErr != nil {
		logger.Warn("failed to look up existing chat", "error", chatErr)
	}

	parts := []codersdk.ChatInputPart{{Type: codersdk.ChatInputPartTypeText, Text: msg}}

	if chatID != uuid.Nil {
		logger.Info("continuing existing chat", "chat_id", chatID)
		if _, err := b.coder.CreateChatMessage(ctx, chatID, codersdk.CreateChatMessageRequest{
			Content:      parts,
			BusyBehavior: codersdk.ChatBusyBehaviorInterrupt,
		}); err != nil {
			logger.Error("failed to send follow-up message", "error", err)
			b.setStatus(ctx, ev.Channel, threadTs, "")
			return
		}
	} else {
		chat, err := b.coder.CreateChat(ctx, codersdk.CreateChatRequest{
			Content:            parts,
			Labels:             map[string]string{"slack_thread": key},
			SystemPrompt:       systemPrompt,
			UnsafeDynamicTools: b.tools,
		})
		if err != nil {
			logger.Error("failed to create chat", "error", err)
			b.setStatus(ctx, ev.Channel, threadTs, "")
			return
		}
		chatID = chat.ID
		logger.Info("chat created", "chat_id", chatID)
	}
}

func (b *bot) findChat(ctx context.Context, key string) (uuid.UUID, error) {
	chats, err := b.coder.ListChats(ctx, &codersdk.ListChatsOptions{
		Labels: map[string]string{"slack_thread": key},
	})
	if err != nil {
		return uuid.Nil, err
	}
	if len(chats) > 0 {
		return chats[0].ID, nil
	}
	return uuid.Nil, nil
}

func (b *bot) buildMessage(ctx context.Context, ev *slackevents.AppMentionEvent, text, threadTs string) string {
	ids := extractMentions(ev.Text)
	allIDs := make([]string, 0, 1+len(ids))
	allIDs = append(allIDs, ev.User)
	for _, id := range ids {
		if id != ev.User {
			allIDs = append(allIDs, id)
		}
	}

	userResults := make(map[string]*slack.User, len(allIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range allIDs {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			u, err := b.lookupUser(ctx, uid)
			if err != nil {
				return
			}
			mu.Lock()
			userResults[uid] = u
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	userName, realName := ev.User, ""
	if u, ok := userResults[ev.User]; ok {
		userName = u.Name
		realName = u.RealName
		if realName == "" {
			realName = u.Profile.DisplayName
		}
	}
	threadLine := threadTs
	if ev.ThreadTimeStamp == "" {
		threadLine = "N/A (new thread)"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "You *must* respond by sending a Slack message.\n"+
		"Slack message metadata:\n\n"+
		"Timestamp: %s\nThread Timestamp: %s\nChannel ID: %s\n"+
		"From User: %s (<@%s>) (%s)\n\n"+
		"You *must* reply using the thread timestamp.\n\n"+
		"Message:\n%s\n",
		ev.TimeStamp, threadLine, ev.Channel, userName, ev.User, realName, text)

	if len(ids) > 0 {
		sb.WriteString("\nMentions:\n")
		for _, id := range ids {
			if id == b.botUID {
				continue
			}
			if u, ok := userResults[id]; ok {
				fmt.Fprintf(&sb, "User: %s => %s (%s)\n", id, u.Name, u.RealName)
			} else {
				fmt.Fprintf(&sb, "User: %s\n", id)
			}
		}
	}
	return sb.String()
}

func extractMentions(text string) []string {
	matches := reMention.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool, len(matches))
	var ids []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	return ids
}

// watchChats connects to the global chat watch stream and handles
// tool call events for chats matching our labels.
func (b *bot) watchChats(ctx context.Context) {
	for ctx.Err() == nil {
		b.logger.Info("connecting to chat watch stream")
		events, closer, err := b.coder.WatchChats(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.logger.Error("failed to connect chat watch stream", "error", err)
			continue
		}

		for event := range events {
			parts := strings.SplitN(event.Chat.Labels["slack_thread"], ":", 2)
			if len(parts) != 2 {
				continue
			}
			channel, threadTs := parts[0], parts[1]
			logger := b.logger.With("chat_id", event.Chat.ID, "channel", channel, "thread_ts", threadTs)

			switch event.Kind {
			case codersdk.ChatWatchEventKindActionRequired:
				if len(event.ToolCalls) == 0 {
					continue
				}
				logger.Info("action_required", "tool_calls", len(event.ToolCalls))
				results := make([]codersdk.ToolResult, len(event.ToolCalls))
				var twg sync.WaitGroup
				for i, c := range event.ToolCalls {
					twg.Add(1)
					go func(idx int, tc codersdk.ChatStreamToolCall) {
						defer twg.Done()
						if err := verifyToolCall(tc.ToolName, tc.Args, channel); err != nil {
							results[idx] = codersdk.ToolResult{ToolCallID: tc.ToolCallID, Output: json.RawMessage(fmt.Sprintf("%q", err.Error())), IsError: true}
							return
						}
						call := codersdk.DynamicToolCall{ToolCallID: tc.ToolCallID, ToolName: tc.ToolName, Args: tc.Args}
						dt, ok := b.toolMap[tc.ToolName]
						if !ok {
							results[idx] = codersdk.ToolResult{ToolCallID: tc.ToolCallID, Output: json.RawMessage(fmt.Sprintf("%q", tc.ToolName)), IsError: true}
							return
						}
						resp, err := dt.Handler(ctx, call)
						if err != nil {
							resp = codersdk.DynamicToolResponse{Content: fmt.Sprintf("tool failed: %v", err), IsError: true}
						}
						outputJSON, err := json.Marshal(resp.Content)
						if err != nil {
							outputJSON = []byte(fmt.Sprintf("%q", resp.Content))
						}
						results[idx] = codersdk.ToolResult{ToolCallID: tc.ToolCallID, Output: json.RawMessage(outputJSON), IsError: resp.IsError}
						if resp.IsError {
							logger.Warn("tool error", "id", tc.ToolCallID, "output", resp.Content)
						}
					}(i, c)
				}
				twg.Wait()
				if err := b.coder.SubmitToolResults(ctx, event.Chat.ID, codersdk.SubmitToolResultsRequest{
					Results: results,
				}); err != nil {
					logger.Error("failed to submit tool results", "error", err)
					continue
				}
				logger.Info("tool results submitted")

			case codersdk.ChatWatchEventKindStatusChange:
				logger.Info("status_change", "status", event.Chat.Status)
				switch event.Chat.Status {
				case codersdk.ChatStatusCompleted, codersdk.ChatStatusError:
					b.setStatus(ctx, channel, threadTs, "")
				}
			}
		}

		closer.Close()
		if ctx.Err() != nil {
			return
		}
		b.logger.Warn("chat watch stream disconnected, reconnecting...")
	}
}

func (b *bot) setStatus(ctx context.Context, channel, threadTs, status string, loading ...string) {
	if err := b.slack.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
		ChannelID:       channel,
		ThreadTS:        threadTs,
		Status:          status,
		LoadingMessages: loading,
	}); err != nil {
		b.logger.Warn("failed to set thread status", "error", err)
	}
}

func formatMessage(text string) (string, bool, int) {
	origLen := len(text)
	truncated := origLen > maxMessageLen

	if truncated {
		text = text[:maxMessageLen]
	}

	text = strings.ReplaceAll(text, `\n`, "\n")
	text = strings.ReplaceAll(text, `\"`, `"`)

	var preserved []string
	ph := func(s string) string {
		idx := len(preserved)
		preserved = append(preserved, s)
		return fmt.Sprintf("\x00CODE%d\x00", idx)
	}
	text = reCodeBlock.ReplaceAllStringFunc(text, ph)
	text = reInlineCode.ReplaceAllStringFunc(text, ph)

	text = reMarkdownLink.ReplaceAllString(text, "<$2|$1>")
	text = reBold.ReplaceAllString(text, "*$1*")
	text = reHeading.ReplaceAllString(text, "*$1*")

	for i, code := range preserved {
		cleaned := reCodeLang.ReplaceAllString(code, "```")
		text = strings.Replace(text, fmt.Sprintf("\x00CODE%d\x00", i), cleaned, 1)
	}

	return text, truncated, origLen
}

// Tool classification for channel pinning. Tools not listed in either map are
// rejected outright, so a tool added later defaults to deny until it is
// explicitly classified (see sendbird_override.md).
var channelBoundTools = map[string]bool{
	"slack_send_message":       true,
	"slack_edit_message":       true,
	"slack_react_to_message":   true,
	"slack_get_thread_replies": true,
	"slack_report_status":      true,
}

var channelFreeTools = map[string]bool{
	"slack_get_user_info": true,
}

// verifyToolCall rejects tool calls that target a channel other than the one
// this chat is pinned to (the channel the bot was mentioned in), so a
// prompt-injected model cannot read from or write to arbitrary channels the
// bot happens to be a member of. Pinning is channel-level only: threads within
// the pinned channel are not restricted.
func verifyToolCall(toolName, args, channel string) error {
	if channelFreeTools[toolName] {
		return nil
	}
	if !channelBoundTools[toolName] {
		return fmt.Errorf("tool %s is not classified for channel pinning: add it to channelBoundTools or channelFreeTools before use", toolName)
	}
	var probe struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		return fmt.Errorf("invalid tool args: %v", err)
	}
	if probe.Channel != channel {
		return fmt.Errorf("channel %q not allowed: this chat is pinned to channel %s", probe.Channel, channel)
	}
	return nil
}

func jsonResponse(v any) (codersdk.DynamicToolResponse, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return codersdk.DynamicToolResponse{}, fmt.Errorf("marshal tool response: %w", err)
	}
	return codersdk.DynamicToolResponse{Content: string(data)}, nil
}
