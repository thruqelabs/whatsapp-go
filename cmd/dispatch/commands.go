// dispatch package provides the core command registry, execution pipeline,
// and dispatch coordinator for the WhatsRook CLI bot application.
//
// it maintains an in-memory synchronized registry of command handlers, parses incoming
// message triggers against configured prefixes, evaluates authorization policies (owner, sudo, admin),
// and executes commands with context cancellation and error handling.
package dispatch

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"whatsrook"
	"whatsrook/util/builder"
	"whatsrook/util/external"
)

// Context aliases the core whatsrook.PluginContext execution context.
type Context = whatsrook.PluginContext

// TextBuilder aliases the fluent text composition builder.
type TextBuilder = builder.TextBuilder

// PollBuilder aliases the interactive poll creation builder.
type PollBuilder = builder.PollBuilder

// PollRequest aliases the structured poll configuration payload.
type PollRequest = builder.PollRequest

// NewText constructs a fluent text message builder.
func NewText(initial ...string) *TextBuilder {
	return whatsrook.NewText(initial...)
}

var (
	// Sprintf is an alias for standard string formatting.
	Sprintf = whatsrook.Sprintf
	// Bold formats plain text as bold.
	Bold = whatsrook.Bold
	// Boldf formats formatted text as bold.
	Boldf = whatsrook.Boldf
	// Italic formats plain text as italic.
	Italic = whatsrook.Italic
	// Italicf formats formatted text as italic.
	Italicf = whatsrook.Italicf
	// Code formats plain text as inline code.
	Code = whatsrook.Code
	// Codef formats formatted text as inline code.
	Codef = whatsrook.Codef
	// CodeBlock formats plain text as a code block.
	CodeBlock = whatsrook.CodeBlock
	// Strike formats plain text with strikethrough.
	Strike = whatsrook.Strike
	// Strikef formats formatted text with strikethrough.
	Strikef = whatsrook.Strikef
	// Quote formats plain text as a blockquote.
	Quote = whatsrook.Quote
	// Quotef formats formatted text as a blockquote.
	Quotef = whatsrook.Quotef
)

// Handler represents the function signature executed when a command is triggered.
type Handler func(ctx *Context) error

// Command defines the metadata, authorization rules, and execution handler for a bot command.
type Command struct {
	// Name is the primary trigger word for the command (e.g. "ping").
	Name string
	// Alias is a comma-separated list of secondary trigger words (e.g. "p,pong").
	Alias string
	// Description provides a concise explanation of what the command does.
	Description string
	// Category groups the command under a menu domain (e.g. "Group", "Owner", "Tools").
	Category string
	// HideFromMenu determines if the command is omitted from the public help menu.
	HideFromMenu bool
	// GroupOnly restricts execution exclusively to group chats.
	GroupOnly bool
	// IsPublic allows regular group members and DM users to execute without sudo privileges.
	IsPublic bool
	// Handler is the function executed when the command matches.
	Handler Handler
}

// CommandInfo provides a lightweight JSON-serializable snapshot of command metadata.
type CommandInfo struct {
	Name        string `json:"name"`
	Alias       string `json:"alias,omitempty"`
	Description string `json:"description"`
	Category    string `json:"category"`
	GroupOnly   bool   `json:"group_only"`
	IsPublic    bool   `json:"is_public"`
}

var (
	// registryMu synchronizes access to the command registry maps.
	registryMu sync.RWMutex
	// registry maps normalized command trigger words to their definitions.
	registry = map[string]*Command{}
	// order preserves the registration sequence of primary command names.
	order = []string{}
)

// Register adds one or more commands to the global dispatch registry.
func Register(commands ...*Command) {
	registryMu.Lock()
	defer registryMu.Unlock()

	for _, c := range commands {
		if c == nil || c.Name == "" {
			continue
		}
		key := strings.ToLower(c.Name)
		if _, exists := registry[key]; !exists {
			order = append(order, c.Name)
		}
		registry[key] = c
		if c.Alias != "" {
			for a := range strings.SplitSeq(c.Alias, ",") {
				a = strings.TrimSpace(a)
				if a != "" {
					registry[strings.ToLower(a)] = c
				}
			}
		}
	}
}

// Get retrieves a command by its primary name or alias.
func Get(name string) (*Command, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	c, ok := registry[strings.ToLower(name)]
	return c, ok
}

// All returns all registered commands in registration order.
func All() []*Command {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]*Command, 0, len(order))
	seen := make(map[*Command]bool)
	for _, name := range order {
		if c, ok := registry[strings.ToLower(name)]; ok && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// Count returns the total number of unique registered commands.
func Count() int {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return len(order)
}

// ClosestCommand returns the registered command name or alias closest in edit distance
// to the provided target string using Damerau-Levenshtein distance.
func ClosestCommand(target string) string {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return ""
	}

	registryMu.RLock()
	defer registryMu.RUnlock()

	var bestMatch string
	bestDist := math.MaxInt
	bestLenDiff := math.MaxInt
	bestIsPrimary := false

	// Check all triggers in registry (both primary command names and aliases)
	for trigger, cmd := range registry {
		dist := damerauLevenshtein(target, trigger)
		isPrimary := cmd != nil && strings.EqualFold(cmd.Name, trigger)
		lenDiff := abs(len(trigger) - len(target))

		if dist < bestDist {
			bestDist = dist
			bestMatch = trigger
			bestLenDiff = lenDiff
			bestIsPrimary = isPrimary
		} else if dist == bestDist {
			// Tie-breaking:
			// 1. Prefer smaller length difference with target
			// 2. Prefer primary command names over aliases
			// 3. Prefer lexicographically smaller trigger for determinism
			if lenDiff < bestLenDiff {
				bestMatch = trigger
				bestLenDiff = lenDiff
				bestIsPrimary = isPrimary
			} else if lenDiff == bestLenDiff {
				if isPrimary && !bestIsPrimary {
					bestMatch = trigger
					bestIsPrimary = isPrimary
				} else if isPrimary == bestIsPrimary && trigger < bestMatch {
					bestMatch = trigger
				}
			}
		}
	}

	// Also check installed external plugins
	if external.DefaultDispatcher != nil {
		if plugins, err := external.DefaultDispatcher.List(); err == nil {
			for _, p := range plugins {
				name := strings.ToLower(p.Name)
				dist := damerauLevenshtein(target, name)
				lenDiff := abs(len(name) - len(target))
				if dist < bestDist {
					bestDist = dist
					bestMatch = name
					bestLenDiff = lenDiff
					bestIsPrimary = true
				} else if dist == bestDist {
					if lenDiff < bestLenDiff {
						bestMatch = name
						bestLenDiff = lenDiff
						bestIsPrimary = true
					} else if lenDiff == bestLenDiff && (!bestIsPrimary || name < bestMatch) {
						bestMatch = name
						bestIsPrimary = true
					}
				}
			}
		}
	}

	return bestMatch
}

// FormatUnknownCommandSuggestion formats the user-facing suggestion when a command is misspelled.
func FormatUnknownCommandSuggestion(userTag, prefix, closestCmd string) string {
	if userTag == "" || userTag == "@" {
		userTag = "@User"
	}
	return fmt.Sprintf("%s that's not quite right, did you mean to run %s%s?", userTag, prefix, closestCmd)
}

// damerauLevenshtein calculates the Damerau-Levenshtein distance between two strings,
// accounting for insertions, deletions, substitutions, and adjacent transpositions.
func damerauLevenshtein(a, b string) int {
	r1, r2 := []rune(a), []rune(b)
	n, m := len(r1), len(r2)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}

	stride := m + 1
	d := make([]int, (n+1)*stride)
	for i := 0; i <= n; i++ {
		d[i*stride] = i
	}
	for j := 0; j <= m; j++ {
		d[j] = j
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if r1[i-1] == r2[j-1] {
				cost = 0
			}
			del := d[(i-1)*stride+j] + 1
			ins := d[i*stride+(j-1)] + 1
			sub := d[(i-1)*stride+(j-1)] + cost

			minVal := min(sub, min(ins, del))
			d[i*stride+j] = minVal

			// Transposition check
			if i > 1 && j > 1 && r1[i-1] == r2[j-2] && r1[i-2] == r2[j-1] {
				if trans := d[(i-2)*stride+(j-2)] + 1; trans < d[i*stride+j] {
					d[i*stride+j] = trans
				}
			}
		}
	}
	return d[n*stride+m]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func isLikelyCommandName(s string) bool {
	if s == "" {
		return false
	}
	r := rune(s[0])
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
