package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/promptster-ai/promptster-teams-cli/internal/capture"
)

const claudeDirsUsage = "usage: promptster-teams claude-dirs <list|add <dir>|remove <dir>>"

// cmdClaudeDirs dispatches `claude-dirs list|add|remove`: extra Claude config
// roots (another CLAUDE_CONFIG_DIR, e.g. a second account a tool runs
// `claude -p` under) whose transcripts capture also reads. The running daemon
// picks a change up on its next poll; see capture/claude_extra_dirs.go.
func cmdClaudeDirs(args []string) int {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add", "remove":
		if len(args) != 2 || args[1] == "" {
			fmt.Fprintln(os.Stderr, claudeDirsUsage)
			return 1
		}
		edit := capture.AddExtraClaudeConfigDir
		if sub == "remove" {
			edit = capture.RemoveExtraClaudeConfigDir
		}
		changed, err := edit(args[1])
		if err != nil {
			printlnIndent(fmt.Sprintf("%s couldn't %s %s: %v", errGlyph, sub, args[1], err))
			return 1
		}
		if !changed {
			why := "is already captured"
			if sub == "remove" {
				why = "was not in the list"
			}
			printlnIndent(fmt.Sprintf("%s nothing to change: %s %s", warnGlyph, args[1], why))
		}
	case "list":
	default:
		fmt.Fprintf(os.Stderr, "unknown claude-dirs subcommand: %s\n%s\n", sub, claudeDirsUsage)
		return 1
	}
	fmt.Println()
	fmt.Println(brandBar("claude-dirs"))
	fmt.Println()
	printClaudeTranscriptRoots()
	fmt.Println()
	return 0
}

// printClaudeTranscriptRoots prints one line per captured Claude config root's
// transcript store, marking a missing one rather than hiding it. Shared with
// doctor so both describe the same set.
func printClaudeTranscriptRoots() {
	for i, root := range capture.ClaudeConfigRoots() {
		extra := ""
		if i > 0 {
			extra = " (extra)"
		}
		dir := filepath.Join(root, "projects")
		if _, err := os.Stat(dir); err == nil {
			printlnIndent(fmt.Sprintf("%s Claude Code transcripts%s: %s", okGlyph, extra, dir))
		} else {
			printlnIndent(fmt.Sprintf("%s Claude Code transcript dir%s not found yet: %s", warnGlyph, extra, dir))
		}
	}
}
