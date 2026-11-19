package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pakornv/quickwit-mcp/internal/quickwitmcp"
)

func main() {
	server, err := quickwitmcp.NewServer(os.Getenv("QUICKWIT_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
