package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPCommand(t *testing.T) {
	h, token := newHarness(t)
	u := h.user("alice", token, t.TempDir())

	out := u.must("mcp")
	if strings.Contains(out, token) {
		t.Fatalf("vx mcp printed the token:\n%s", out)
	}
	url := h.server + "/mcp"
	for _, want := range []string{
		`"url": "` + url + `"`, `"type": "http"`, `"Authorization": "Bearer ${VX_TOKEN}"`,
		"claude mcp add --transport http vx " + url + " --header 'Authorization: Bearer ${VX_TOKEN}'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("vx mcp output lacks %q:\n%s", want, out)
		}
	}

	var got struct {
		URL     string `json:"url"`
		MCPJSON struct {
			MCPServers map[string]struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"mcpServers"`
		} `json:"mcp_json"`
		Add string `json:"claude_mcp_add"`
	}
	raw := u.must("mcp", "--json", "--name", "vx-dev")
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("vx mcp --json: %v\n%s", err, raw)
	}
	s := got.MCPJSON.MCPServers["vx-dev"]
	if got.URL != url || s.URL != url || s.Type != "http" || s.Headers["Authorization"] != "Bearer ${VX_TOKEN}" || !strings.Contains(got.Add, " vx-dev ") {
		t.Fatalf("vx mcp --json = %+v", got)
	}
	if strings.Contains(raw, token) {
		t.Fatal("vx mcp --json printed the token")
	}
	if out, code := u.run("mcp", "--name", "bad name"); code == 0 || !strings.Contains(out, "--name") {
		t.Fatalf("bad --name: exit %d, %s", code, out)
	}
}
