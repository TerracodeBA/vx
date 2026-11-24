package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// mcpTokenRef is how the MCP config refers to the token: Claude Code
// expands it from the environment when it connects, so the secret is never
// written to a config file.
const mcpTokenRef = "Bearer ${VX_TOKEN}"

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (a *App) mcpCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Show how to connect Claude Code to the vx MCP endpoint",
		Long: `Print how to connect Claude Code, or any MCP client, to vx-server's MCP
endpoint, so agents can query efforts, changes and the board with tools
(vx_status, vx_log, vx_show, vx_diff, vx_context, vx_board, ...) instead of
parsing CLI output. Landing stays in the CLI.

The config refers to the token as ${VX_TOKEN}, which Claude Code expands from
the environment when it connects. No token is printed or written anywhere.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !mcpServerName.MatchString(name) {
				return fmt.Errorf("--name %q: use letters, digits, dash or underscore", name)
			}
			server := strings.TrimRight(a.settings().Server, "/")
			placeholder := server == ""
			if placeholder {
				server = "https://vx.example.com"
			}
			url := server + "/mcp"
			entry := map[string]any{
				"type":    "http",
				"url":     url,
				"headers": map[string]string{"Authorization": mcpTokenRef},
			}
			snippet, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{name: entry}}, "", "  ")
			if err != nil {
				return err
			}
			add := "claude mcp add --transport http " + name + " " + url + " --header 'Authorization: " + mcpTokenRef + "'"
			if placeholder {
				a.printf("Not logged in, so the server below is a placeholder. Run `vx login` or set VX_SERVER first.\n\n")
			}
			a.printf("vx serves MCP at %s (streamable HTTP, vx token as a bearer token).\n\n", url)
			a.printf("To share it with everyone on the project, add this to .mcp.json in the repository root:\n\n%s\n\n", snippet)
			a.printf("Or add it just for yourself (the single quotes keep ${VX_TOKEN} for Claude Code to expand;\nadd --scope user to use it in every project):\n\n  %s\n\n", add)
			a.printf("Then start Claude Code with VX_TOKEN set to the agent's token, for example from\n`vx agent add NAME --env-file FILE` and `source FILE`. Check with /mcp inside Claude Code.\n")
			a.emit(map[string]any{
				"url": url, "server_name": name, "mcp_json": json.RawMessage(snippet), "claude_mcp_add": add,
				"placeholder_server": placeholder,
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "vx", "name of the MCP server in Claude Code")
	return cmd
}
