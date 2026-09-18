// Response parsing and command-instruction generation for Meta AI requests.
package ai

import (
	"strings"

	"go.mau.fi/whatsmeow/types"

	"whatsrook"
)

const metaAiSystemPrompt = `[SYSTEM CONTEXT:
You are {NAME}, a helpful, intelligent, and capable AI assistant on WhatsApp.
The bot has registered commands available to perform actions. When the user asks to run a command, execute an action, or confirms with 'yes' (for example: "run the menu command", "open menu", "show commands", "check ping", "search repo", "yes"), you must trigger the command by responding with EXACTLY:
RUN_COMMAND: {PREFIX}<command_name> [args]
(with no other text, markdown, or commentary).

When the user is chatting, asking a general knowledge question, or having a conversation, answer naturally, thoughtfully, and directly.
Do NOT use emojis anywhere in your responses.
When referencing commands in natural conversation, always use the active prefix '{PREFIX}' (for example: "{PREFIX}menu", "{PREFIX}help", "{PREFIX}ping").
Address the user by their display name.

Available bot commands:
{{COMMANDS_LIST}}
]`

// CommandInfo mirrors commands.CommandInfo — kept as a separate type here
// so meta has no import dependency on the commands package (which
// would create an import cycle, since commands imports meta).
type CommandInfo struct {
	Name        string
	Alias       string
	Description string
	IsPublic    bool
}

// BuildRunCommandInstruction builds the instruction block prepended to
// every Meta AI request, listing the bot's actual registered commands so
// Meta AI can both (a) decide to invoke one via RUN_COMMAND, and (b)
// answer questions about how to use a command, using real data instead
// of guessing.
func BuildRunCommandInstructionWithNameAndPrefix(cmds []CommandInfo, botName, prefix string) string {
	if botName == "" {
		botName = "WhatsRook"
	}
	if prefix == "" {
		prefix = "!"
	}
	promptTmpl := metaAiSystemPrompt

	promptTmpl = strings.ReplaceAll(promptTmpl, "{NAME}", botName)
	promptTmpl = strings.ReplaceAll(promptTmpl, "WhatsRook", botName)
	promptTmpl = strings.ReplaceAll(promptTmpl, "{PREFIX}", prefix)

	cmdsTb := whatsrook.NewText()
	for _, c := range cmds {
		aliasStr := ""
		if c.Alias != "" {
			aliasStr = whatsrook.Sprintf(" (alias: %s%s)", prefix, c.Alias)
		}
		sudoStr := ""
		if !c.IsPublic {
			sudoStr = " [sudo-only]"
		}
		desc := c.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		cmdsTb.Linef("- %s%s%s%s: %s", prefix, c.Name, aliasStr, sudoStr, desc)
	}

	res := strings.ReplaceAll(promptTmpl, "{{COMMANDS_LIST}}", cmdsTb.Trimmed())
	return res + "\n\n"
}

// ParseRunCommand checks whether an AI reply is requesting that the bot
// run one of its own registered commands, using the convention:
//
//	RUN_COMMAND: <prefix><command_name> [args...]
//
// It returns the command name (lowercased) and its raw argument string,
// and ok=true if the reply matched this convention. This only recognizes
// the fixed marker text — it does not interpret, generate, or execute
// anything itself; the caller is responsible for looking the command name
// up in its own registry and deciding whether to run it.
func ParseRunCommand(reply string) (cmdName string, rawArgs string, ok bool) {
	cleaned := strings.TrimSpace(reply)
	cleaned = strings.Trim(cleaned, "` \n\r\t")
	cmdContent, found := strings.CutPrefix(cleaned, "RUN_COMMAND:")
	if !found {
		return "", "", false
	}

	cmdLine := strings.TrimSpace(cmdContent)
	cmdLine = strings.ReplaceAll(cmdLine, "(link unavailable)", "")
	cmdLine = strings.ReplaceAll(cmdLine, "link unavailable", "")
	cmdLine = strings.Trim(cmdLine, "` \n\r\t")
	cmdLine = strings.TrimLeft(cmdLine, ".!/# ")

	fields := strings.Fields(cmdLine)
	if len(fields) == 0 {
		return "", "", false
	}

	cmdName = strings.ToLower(fields[0])
	rawArgs = strings.TrimSpace(cmdLine[len(fields[0]):])
	return cmdName, rawArgs, true
}

// RenderGroupContext turns GroupInfo into a text block appended to the
// query sent to Meta AI, so it has context about the group without a
// live API call on every message (the caller is expected to have already
// fetched/cached info via GetOrFetchGroupMeta).
func RenderGroupContext(info types.GroupInfo) string {
	name := strings.TrimSpace(info.GroupName.Name)
	if name == "" && info.JID.IsEmpty() && info.ParticipantCount == 0 {
		return ""
	}

	tb := whatsrook.NewText().
		Line("[GROUP CONTEXT]")

	if name != "" {
		tb.Linef("Group Name: %s", name)
	}

	if topic := strings.TrimSpace(info.GroupTopic.Topic); topic != "" {
		if len(topic) > 150 {
			topic = topic[:147] + "..."
		}
		tb.Linef("Group Description: %s", topic)
	}
	if info.ParticipantCount > 0 {
		tb.Linef("Participant Count: %d", info.ParticipantCount)
	}

	tb.Line("[/GROUP CONTEXT]").Blank()
	return tb.String()
}

// RenderUserContext turns user info into a text block appended to the query sent to Meta AI.
func RenderUserContext(d Data) string {
	displayName := strings.TrimSpace(d.PushName)
	if displayName == "" {
		displayName = "User"
	}

	tb := whatsrook.NewText().
		Line("[USER CONTEXT]").
		Linef("User: %s", displayName)

	if d.IsSudo {
		tb.Line("Status: Owner/Sudo")
	}
	tb.Line("Instruction: Address the user in conversation using their name above. Do not output or address them using technical IDs, phone numbers, JIDs, or LIDs.").
		Line("[/USER CONTEXT]").
		Blank()

	return tb.String()
}

// RenderQuotedContext turns quoted-message info on Data into a text block
// giving Meta AI context about what message the user is replying to, if any.
func RenderQuotedContext(d Data) string {
	if d.QuotedMessageOfQuestion == "" && d.QuotedMessageType == "" {
		return ""
	}

	tb := whatsrook.NewText().
		Line("[REPLYING TO A MESSAGE — EXTRACTED CONTEXT]")

	if d.UserOfQuotedMessage != "" {
		if d.QuotedMessageParticipantRole != "" {
			tb.Linef("From: %s (%s)", d.UserOfQuotedMessage, d.QuotedMessageParticipantRole)
		} else {
			tb.Linef("From: %s", d.UserOfQuotedMessage)
		}
	}
	if d.QuotedMessageType != "" {
		tb.Linef("Message Type: %s", d.QuotedMessageType)
	}
	if d.QuotedMessageOfQuestion != "" {
		msgContent := d.QuotedMessageOfQuestion
		if len(msgContent) > 500 {
			msgContent = msgContent[:497] + "..."
		}
		tb.Linef("Message Content: %s", msgContent)
	}
	tb.Line("[/REPLYING TO A MESSAGE — EXTRACTED CONTEXT]").Blank()

	return tb.String()
}

// RenderCurrentMessage formats the triggering user's current query or message
// so that Meta AI can clearly distinguish dialogue turns and replying context.
func RenderCurrentMessage(d Data) string {
	cleanQuestion := strings.TrimSpace(d.Question)
	displayName := strings.TrimSpace(d.PushName)
	if displayName == "" {
		displayName = "User"
	}

	tb := whatsrook.NewText().
		Line("[CURRENT MESSAGE]")

	tb.Linef("From: %s", displayName)
	if cleanQuestion != "" {
		tb.Linef("Message: %s", cleanQuestion)
	} else {
		tb.Line("Instruction: The user called the bot to respond to the quoted message above. Provide a helpful, direct response.")
	}
	if d.QuotedMessageOfQuestion != "" {
		tb.Line("Note: The user is replying to the quoted message referenced above in this chat.")
	}
	tb.Line("[/CURRENT MESSAGE]").Blank()

	return tb.String()
}

// BuildAiQuery compiles instructions, personality prompts, group context,
// quoted message context, and user prompt into a structured query for Meta AI.
func BuildAiQuery(instruction, customPrompt string, data Data) string {
	if isMediaGenerationPrompt(data.Question) {
		return data.Question
	}

	var b strings.Builder
	b.WriteString(instruction)

	if customPrompt != "" {
		b.WriteString("\n[GLOBAL BOT PERSONALITY & RELATIONSHIP BEHAVIOR INSTRUCTION]\n")
		b.WriteString(customPrompt)
		b.WriteString("\n\n")
	}

	if data.ChatType == "group" {
		if grp := RenderGroupContext(data.GroupMetaData); grp != "" {
			b.WriteString(grp)
		}
	}

	if usr := RenderUserContext(data); usr != "" {
		b.WriteString(usr)
	}

	if qtd := RenderQuotedContext(data); qtd != "" {
		b.WriteString(qtd)
	}

	if cur := RenderCurrentMessage(data); cur != "" {
		b.WriteString(cur)
	}

	return b.String()
}

// CleanAiResponseText strips any echoed system prompts, context scaffolding, or prompt delimiters from the AI response.
func CleanAiResponseText(text string) string {
	cleaned := text

	// 1. Strip [SYSTEM CONTEXT: ... ] if echoed by the LLM
	if start := strings.Index(cleaned, "[SYSTEM CONTEXT:"); start != -1 {
		sub := cleaned[start:]
		if _, after, ok := strings.Cut(sub, "\n]"); ok {
			cleaned = cleaned[:start] + after
		} else if _, after, ok := strings.Cut(sub, "]"); ok {
			cleaned = cleaned[:start] + after
		}
	}

	// 2. Strip [GLOBAL BOT PERSONALITY & RELATIONSHIP BEHAVIOR INSTRUCTION]
	if start := strings.Index(cleaned, "[GLOBAL BOT PERSONALITY & RELATIONSHIP BEHAVIOR INSTRUCTION]"); start != -1 {
		sub := cleaned[start:]
		if _, after, ok := strings.Cut(sub, "\n\n"); ok {
			cleaned = cleaned[:start] + after
		}
	}

	// 3. Strip [CURRENT MESSAGE] ... [/CURRENT MESSAGE]
	if start := strings.Index(cleaned, "[CURRENT MESSAGE]"); start != -1 {
		if end := strings.Index(cleaned, "[/CURRENT MESSAGE]"); end != -1 {
			cleaned = cleaned[:start] + cleaned[end+len("[/CURRENT MESSAGE]"):]
		}
	}

	// 4. Strip [GROUP CONTEXT] ... [/GROUP CONTEXT]
	if start := strings.Index(cleaned, "[GROUP CONTEXT]"); start != -1 {
		if end := strings.Index(cleaned, "[/GROUP CONTEXT]"); end != -1 {
			cleaned = cleaned[:start] + cleaned[end+len("[/GROUP CONTEXT]"):]
		}
	}

	// 5. Strip [USER CONTEXT] ... [/USER CONTEXT]
	if start := strings.Index(cleaned, "[USER CONTEXT]"); start != -1 {
		if end := strings.Index(cleaned, "[/USER CONTEXT]"); end != -1 {
			cleaned = cleaned[:start] + cleaned[end+len("[/USER CONTEXT]"):]
		}
	}

	// 6. Strip [REPLYING TO A MESSAGE — EXTRACTED CONTEXT] ... [/REPLYING TO A MESSAGE — EXTRACTED CONTEXT]
	if start := strings.Index(cleaned, "[REPLYING TO A MESSAGE — EXTRACTED CONTEXT]"); start != -1 {
		if end := strings.Index(cleaned, "[/REPLYING TO A MESSAGE — EXTRACTED CONTEXT]"); end != -1 {
			cleaned = cleaned[:start] + cleaned[end+len("[/REPLYING TO A MESSAGE — EXTRACTED CONTEXT]"):]
		}
	}

	// 7. Strip [QUOTED MESSAGE] ... [/QUOTED MESSAGE]
	if start := strings.Index(cleaned, "[QUOTED MESSAGE]"); start != -1 {
		if end := strings.Index(cleaned, "[/QUOTED MESSAGE]"); end != -1 {
			cleaned = cleaned[:start] + cleaned[end+len("[/QUOTED MESSAGE]"):]
		}
	}

	// 8. Strip any stray context tags
	strayTags := []string{
		"[/CURRENT MESSAGE]",
		"[/GROUP CONTEXT]",
		"[/USER CONTEXT]",
		"[/REPLYING TO A MESSAGE — EXTRACTED CONTEXT]",
		"[/QUOTED MESSAGE]",
		"[CURRENT MESSAGE]",
		"[GROUP CONTEXT]",
		"[USER CONTEXT]",
		"[REPLYING TO A MESSAGE — EXTRACTED CONTEXT]",
		"[QUOTED MESSAGE]",
	}
	for _, tag := range strayTags {
		cleaned = strings.ReplaceAll(cleaned, tag, "")
	}

	return strings.TrimSpace(cleaned)
}
