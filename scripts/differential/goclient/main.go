// Command goclient relays tool calls to two MCP servers through the go-sdk client,
// for the differential harness. Each stdin line is {"tool": ..., "args": {...}}; each
// stdout line is {"py": answer, "go": answer} in the harness's comparison shape.
//
// Usage: goclient <protocol version, or "" for go-sdk's default> <url> <url>
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type answer struct {
	Raised     string      `json:"raised,omitempty"`
	IsError    bool        `json:"isError"`
	Structured any         `json:"structured"`
	Content    [][2]string `json:"content"`
}

func call(ctx context.Context, s *mcp.ClientSession, tool string, args json.RawMessage) answer {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return answer{Raised: err.Error()}
	}
	a := answer{IsError: res.IsError, Structured: res.StructuredContent, Content: [][2]string{}}
	for _, c := range res.Content {
		text := ""
		if t, ok := c.(*mcp.TextContent); ok {
			text = t.Text
		}
		b, _ := json.Marshal(c)
		var kind struct{ Type string }
		json.Unmarshal(b, &kind)
		a.Content = append(a.Content, [2]string{kind.Type, text})
	}
	return a
}

func main() {
	ctx := context.Background()
	version, urls := os.Args[1], os.Args[2:]
	var sessions []*mcp.ClientSession
	for _, u := range urls {
		client := mcp.NewClient(&mcp.Implementation{Name: "differential", Version: "1"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: u}, &mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			log.Fatalf("%s: %v", u, err)
		}
		defer s.Close()
		sessions = append(sessions, s)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<24)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var op struct {
			Tool string
			Args json.RawMessage
		}
		if err := json.Unmarshal(in.Bytes(), &op); err != nil {
			log.Fatal(err)
		}
		if err := out.Encode(map[string]answer{
			"py": call(ctx, sessions[0], op.Tool, op.Args),
			"go": call(ctx, sessions[1], op.Tool, op.Args),
		}); err != nil {
			log.Fatal(err)
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
