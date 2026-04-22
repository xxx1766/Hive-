// Package netproxy handles Agent→Hive net/fetch requests.
//
// This is where "shared connections, isolated quotas" (ARCHITECTURE.md §140)
// materialises: all Rooms and Agents share a single http.Transport (so TCP
// connections and TLS handshakes are pooled per origin), but each (Room,
// Agent) has its own quota counter in the quota.Actor. One Agent running
// out of its budget doesn't affect anyone else's budget or the pool.
package netproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

// sharedClient is process-wide. It is what makes connection reuse observable
// via `ss -tnp` when two Agents hit the same host.
var sharedClient = func() *http.Client {
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: t, Timeout: 30 * time.Second}
}()

// SharedClient exposes the process-wide HTTP client. Other proxies (llmproxy)
// share this so that "one connection pool per provider key" is a side effect
// of code layout, not a separate abstraction.
func SharedClient() *http.Client { return sharedClient }

// Proxy is built per-Agent. Holding Quota+RoomID+AgentName here keeps the
// handler method signatures thin.
type Proxy struct {
	RoomID    string
	AgentName string
	Rank      *rank.Rank
	Quota     *quota.Actor
}

// Fetch performs an HTTP request on behalf of the Agent.
func (p *Proxy) Fetch(ctx context.Context, params json.RawMessage) (any, error) {
	var fp rpc.NetFetchParams
	if err := json.Unmarshal(params, &fp); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if !p.Rank.NetAllowed {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot use net/fetch")
	}

	// Per-Agent quota: 1 API call per fetch.
	key := quota.Key{RoomID: p.RoomID, Agent: p.AgentName, Resource: "http"}
	res, err := p.Quota.Consume(ctx, key, 1)
	if err != nil {
		return nil, err
	}
	if !res.Allowed {
		return nil, protocol.ErrQuotaExceeded(fmt.Sprintf(
			"http quota exhausted (remaining=%d)", res.Remaining))
	}

	method := fp.Method
	if method == "" {
		method = "GET"
	}
	var body io.Reader
	if len(fp.Body) > 0 {
		body = bytes.NewReader(fp.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fp.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range fp.Headers {
		req.Header.Set(k, v)
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http fetch: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	headers := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	return rpc.NetFetchResult{Status: resp.StatusCode, Headers: headers, Body: b}, nil
}

// Package-level guard to ensure sharedClient isn't accidentally replaced.
var _ sync.Once // no-op, kept for future thread-safety annotations
