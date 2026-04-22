// upper is a staff-rank Agent that uppercases text it receives, either
// via task/run or from a peer Agent (peer/recv). Exercises the peer
// message channel in the SDK.
package main

import (
	"context"
	"encoding/json"
	"strings"

	hive "github.com/anne-x/hive/sdk/go"
)

func main() {
	a := hive.MustConnect()
	defer a.Close()

	for {
		select {
		case task, ok := <-a.Tasks():
			if !ok {
				return
			}
			text := extractText(task.Input)
			a.Log("info", "uppercasing", map[string]any{"len": len(text)})
			task.Reply(map[string]any{"text": strings.ToUpper(text)})

		case peer, ok := <-a.Peers():
			if !ok {
				return
			}
			text := extractText(peer.Payload)
			// Reply to the peer with the uppercased result.
			_ = a.PeerSend(context.Background(), peer.From, map[string]any{
				"text": strings.ToUpper(text),
			})

		case <-a.Done():
			return
		}
	}
}

// extractText tolerates either a raw JSON string or {text: "..."}.
func extractText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Text
	}
	return string(raw)
}
