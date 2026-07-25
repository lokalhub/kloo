package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/mcp"
	"github.com/lokalhub/kloo/internal/tools"
)

func memoryRecall(ctx context.Context, cfg config.Config, mgr *mcp.Manager, cwd, task string, logf func(string, ...any)) string {
	mem := cfg.Memory
	if !mem.Enabled || mem.Server == "" || mem.RecallTool == "" {
		return ""
	}
	res, err := mgr.Call(ctx, mem.Server, mem.RecallTool, map[string]any{
		"workspace": cwd,
		"task":      task,
		"model":     cfg.Model,
		"endpoint":  cfg.Endpoint,
	})
	if err != nil {
		if logf != nil {
			logf("kloo: memory · recall skipped: %s (run continues)", redactMemoryHookError(err, cfg))
		}
		return ""
	}
	limit := mem.MaxRecallBytes
	if limit <= 0 {
		limit = 4096
		// #3 scaled cheat-sheet (opt-in, KLOO_RECALL_SCALE=1): scale the recall
		// budget with the curator context window (~half the window in bytes ≈12%
		// of tokens) instead of the fixed 4KB, so a large-context model holds the
		// whole guide. Off by default → stock behavior; independent on/off switch.
		if v := os.Getenv("KLOO_RECALL_SCALE"); v != "" && v != "0" {
			if scaled := cfg.MaxContextTokens / 2; scaled > limit {
				limit = scaled
			}
		}
	}
	return boundedString(redactConfiguredSecrets(res.Output, cfg), limit)
}

func memoryRecallSystemSection(recall string) string {
	if recall == "" {
		return ""
	}
	return "\n\nExternal memory recall:\n" + recall
}

func memoryStore(ctx context.Context, cfg config.Config, mgr *mcp.Manager, cwd, task string, summary runSummary, rep *agent.Report, logf func(string, ...any)) {
	mem := cfg.Memory
	if !mem.Enabled || mem.Server == "" || mem.StoreTool == "" {
		return
	}
	if summary.FailureCode != "" && !mem.StoreOnFailure {
		return
	}
	args := map[string]any{
		"version":       1,
		"workspace":     cwd,
		"task":          task,
		"model":         cfg.Model,
		"endpoint":      cfg.Endpoint,
		"success":       summary.Success,
		"reason":        summary.Reason,
		"failure_code":  summary.FailureCode,
		"verify":        summary.Verify,
		"touched_files": touchedFilesFromReport(rep),
	}
	if _, err := mgr.Call(ctx, mem.Server, mem.StoreTool, args); err != nil && logf != nil {
		logf("kloo: memory · store skipped: %s (run continues)", redactMemoryHookError(err, cfg))
	}
}

func redactMemoryHookError(err error, cfg config.Config) string {
	if err == nil {
		return ""
	}
	return redactConfiguredSecrets(err.Error(), cfg)
}

func redactConfiguredSecrets(msg string, cfg config.Config) string {
	for _, secret := range memoryHookSecretValues(cfg) {
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	return msg
}

func memoryHookSecretValues(cfg config.Config) []string {
	seen := map[string]bool{}
	var secrets []string
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		secrets = append(secrets, v)
	}
	add(cfg.APIKey)
	for _, srv := range cfg.MCPServers {
		for _, v := range srv.Env {
			add(v)
		}
		for _, v := range srv.Headers {
			add(v)
		}
	}
	return secrets
}

func touchedFilesFromReport(rep *agent.Report) []string {
	if rep == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, msg := range rep.Transcript {
		for _, tc := range msg.ToolCalls {
			switch tc.Function.Name {
			case tools.NameReadFile, tools.NameEditFile, tools.NameWriteFile:
				if p := toolCallPath(tc); p != "" {
					seen[p] = true
				}
			}
		}
	}
	files := make([]string, 0, len(seen))
	for p := range seen {
		files = append(files, filepath.ToSlash(p))
	}
	sort.Strings(files)
	return files
}

func toolCallPath(tc llm.ToolCall) string {
	var args map[string]any
	if err := jsonUnmarshalString(tc.Function.Arguments, &args); err != nil {
		return ""
	}
	p, _ := args["path"].(string)
	return p
}

func jsonUnmarshalString(s string, v any) error {
	if s == "" {
		return fmt.Errorf("empty json")
	}
	return json.Unmarshal([]byte(s), v)
}
