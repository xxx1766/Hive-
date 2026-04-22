// Echo is the simplest possible Hive Agent: on task/run, it replies with
// task/done whose output is whatever input it got. Lives at the raw
// JSON-RPC layer so it also serves as a reference for what the Go SDK
// (M5) will wrap.
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func main() {
	rd := bufio.NewReaderSize(os.Stdin, 64*1024)
	wr := bufio.NewWriter(os.Stdout)
	defer wr.Flush()

	send := func(v any) {
		b, _ := json.Marshal(v)
		wr.Write(b)
		wr.WriteByte('\n')
		wr.Flush()
	}

	for {
		line, err := rd.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		var m msg
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		switch m.Method {
		case "task/run":
			// Ack the request so Hive's Conn.Call unblocks.
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": struct{}{}})

			var p struct {
				TaskID string          `json:"task_id"`
				Input  json.RawMessage `json:"input,omitempty"`
			}
			_ = json.Unmarshal(m.Params, &p)

			// Emit a log line so the room/log stream has something visible.
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "log",
				"params": map[string]any{
					"level": "info",
					"msg":   "echoing input",
				},
			})

			// Send task/done as a notification — fire-and-forget.
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "task/done",
				"params": map[string]any{
					"task_id": p.TaskID,
					"output":  p.Input,
				},
			})

		case "shutdown":
			return

		default:
			if len(m.ID) > 0 {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      m.ID,
					"error":   map[string]any{"code": -32601, "message": "method not found: " + m.Method},
				})
			}
		}
	}
}
