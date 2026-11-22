package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/shaul/mesh/internal/mcpwire"
)

// maxToolPages bounds the tools/list pages read to compile header rules.
const maxToolPages = 20

// connectExternal opens an external loopback HTTP server. Mesh owns no
// process for it, so a generation is one probed connection with that
// account's credentials and the tool schemas used to mirror headers.
func (s *Supervisor) connectExternal(ctx context.Context, inst *instance, d Definition) (*generation, error) {
	headers, err := secretHeaders(d)
	if err != nil {
		return nil, s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	s.mu.Lock()
	inst.rec.Generation++
	inst.rec.Revision, inst.rec.Intent, inst.rec.State, inst.rec.Reason, inst.rec.ExitCode, inst.rec.Worker = d.Revision, intentRun, StateStarting, "", nil, nil
	gen := &generation{number: inst.rec.Generation, revision: d.Revision}
	s.mu.Unlock()
	if err := s.persist(ctx, inst); err != nil {
		return nil, err
	}
	budget := &byteBudget{limit: instanceQueueBytes, parent: s.router.budget}
	upstream, err := DialHTTP(HTTPUpstreamConfig{URL: d.URL, Headers: headers, ToolHeaders: gen.toolHeaders, budget: budget})
	if err != nil {
		return nil, s.settle(ctx, inst, StateFailed, "connect to "+d.URL+": "+err.Error(), nil)
	}
	gen.upstream = upstream
	if gen.serverInfo, err = s.probe(ctx, upstream, d.ReadyTimeout); err != nil {
		_ = upstream.Close()
		return nil, s.failExternal(ctx, inst, err)
	}
	if err := s.refreshTools(ctx, gen); err != nil {
		_ = upstream.Close()
		return nil, s.settle(ctx, inst, StateFailed, err.Error(), nil)
	}
	if err := s.settle(ctx, inst, StateReady, "", nil); err != nil {
		_ = upstream.Close()
		return nil, err
	}
	return gen, nil
}

func (s *Supervisor) failExternal(ctx context.Context, inst *instance, err error) error {
	var f *Failure
	if errors.As(err, &f) && f.Code == codeIncompatible {
		return s.settle(ctx, inst, StateIncompatible, f.Err.Error(), nil)
	}
	return s.settle(ctx, inst, StateFailed, err.Error(), nil)
}

// secretHeaders reads each credential header from its secret file. The
// values exist only in this request's memory and the H5 client.
func secretHeaders(d Definition) (http.Header, error) {
	headers := http.Header{}
	for name, path := range d.HeaderSecrets {
		value, err := readSecret(path)
		if err != nil {
			return nil, fmt.Errorf("secret header %s: %w", name, err)
		}
		headers.Set(name, value)
	}
	return headers, nil
}

func (g *generation) toolHeaders(tool string) (mcpwire.ParameterHeaders, bool) {
	tools := g.tools.Load()
	if tools == nil {
		return mcpwire.ParameterHeaders{}, false
	}
	return tools.Lookup(tool)
}

// refreshTools reads the complete tool listing and compiles its x-mcp-header
// rules, so tools/call mirrors exactly the parameters the schema declares.
func (s *Supervisor) refreshTools(ctx context.Context, gen *generation) error {
	var tools []json.RawMessage
	cursor := ""
	for range maxToolPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		reply, err := s.hostCall(ctx, gen.upstream, mcpwire.MethodListTools, params)
		if err != nil {
			return fmt.Errorf("list tools: %w", err)
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if reply.ErrorObject() != nil || json.Unmarshal(reply.Result(), &page) != nil {
			return errors.New("list tools: the server refused tools/list")
		}
		tools = append(tools, page.Tools...)
		if cursor = page.NextCursor; cursor == "" {
			return gen.storeTools(tools)
		}
	}
	return fmt.Errorf("list tools: more than %d pages", maxToolPages)
}

func (g *generation) storeTools(tools []json.RawMessage) error {
	raw, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		return fmt.Errorf("compile tool headers: %w", err)
	}
	compiled, err := CompileToolHeaders(raw)
	if err != nil {
		return err
	}
	g.tools.Store(&compiled)
	return nil
}
