// fetch is an intern-rank Agent: given a URL, pulls the body via Hive's
// HTTP proxy. Demonstrates Rank=intern can use net/fetch with a small
// api_calls quota.
package main

import (
	"context"
	"encoding/json"

	hive "github.com/anne-x/hive/sdk/go"
)

type input struct {
	URL string `json:"url"`
}

type output struct {
	Status int    `json:"status"`
	Size   int    `json:"size"`
	Head   string `json:"head"` // first 200 bytes, for demo legibility
}

func main() {
	a := hive.MustConnect()
	defer a.Close()

	ctx := context.Background()

	for task := range a.Tasks() {
		var in input
		if err := json.Unmarshal(task.Input, &in); err != nil {
			a.Log("error", "bad input", map[string]any{"err": err.Error()})
			task.Fail(1, "bad input: "+err.Error())
			continue
		}
		a.Log("info", "fetching", map[string]any{"url": in.URL})

		status, body, err := a.NetFetch(ctx, "GET", in.URL, nil, nil)
		if err != nil {
			a.Log("error", "fetch failed", map[string]any{"err": err.Error()})
			task.Fail(2, err.Error())
			continue
		}

		head := string(body)
		if len(head) > 200 {
			head = head[:200]
		}
		task.Reply(output{Status: status, Size: len(body), Head: head})
	}
}
