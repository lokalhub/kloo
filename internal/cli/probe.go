package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
	"github.com/spf13/cobra"
)

type probeCheck struct {
	OK          bool   `json:"ok"`
	FailureCode string `json:"failure_code,omitempty"`
	Message     string `json:"message,omitempty"`
}

type probeChecks struct {
	ToolCall probeCheck `json:"tool_call"`
	FileEdit probeCheck `json:"file_edit"`
	JSONOnly probeCheck `json:"json_only"`
}

type probeResult struct {
	Model              string      `json:"model"`
	Endpoint           string      `json:"endpoint"`
	ToolFormat         string      `json:"tool_format"`
	OK                 bool        `json:"ok"`
	Checks             probeChecks `json:"checks"`
	TempWorkspace      string      `json:"temp_workspace,omitempty"`
	TempWorkspaceClean bool        `json:"temp_workspace_removed"`
}

func newProbeCmd(deps *Deps) *cobra.Command {
	values := configFlagValues{}
	cmd := &cobra.Command{
		Use:           "probe",
		Short:         "Run cheap model capability checks in a temporary workspace",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			flags, err := buildConfigFlagsFromCommand(cmd, values)
			if err != nil {
				return err
			}
			cfg, err := config.Resolve(flags, deps.Getenv, values.Profile)
			if err != nil {
				return err
			}
			res := runProbe(cmd.Context(), cfg)
			if values.JSON {
				if err := writeProbeJSON(deps.Out, res); err != nil {
					return err
				}
			} else {
				writeProbeHuman(deps.Out, res)
			}
			return nil
		},
	}
	addConfigFlags(cmd.Flags(), &values)
	return cmd
}

func runProbe(ctx context.Context, cfg config.Config) probeResult {
	res := probeResult{
		Model:      cfg.Model,
		Endpoint:   cfg.Endpoint,
		ToolFormat: cfg.ToolFormat,
	}
	root, err := os.MkdirTemp("", "kloo-probe-*")
	if err != nil {
		chk := probeFail("internal_error", err)
		res.Checks = probeChecks{ToolCall: chk, FileEdit: chk, JSONOnly: chk}
		return finishProbeResult(res)
	}
	res.TempWorkspace = root

	ws, err := tools.NewWorkspace(root)
	if err != nil {
		chk := probeFail("internal_error", err)
		res.Checks = probeChecks{ToolCall: chk, FileEdit: chk, JSONOnly: chk}
		return finishProbeResult(res)
	}
	if err := os.WriteFile(filepath.Join(root, "probe.txt"), []byte("before\n"), 0o644); err != nil {
		chk := probeFail("internal_error", err)
		res.Checks = probeChecks{ToolCall: chk, FileEdit: chk, JSONOnly: chk}
		return finishProbeResult(res)
	}

	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		chk := probeFail("config_error", err)
		res.Checks = probeChecks{ToolCall: chk, FileEdit: chk, JSONOnly: chk}
		return finishProbeResult(res)
	}
	res.ToolFormat = adapterName(adapter)
	reg := tools.DefaultRegistry(ws)
	client := llm.New(cfg.Endpoint, cfg.Model,
		llm.WithAPIKey(cfg.APIKey),
		llm.WithTimeout(cfg.LLMColdLoadTimeout),
		llm.WithStreamIdleTimeout(cfg.LLMStreamIdleTimeout),
	)

	res.Checks.ToolCall = probeToolCall(ctx, cfg, client, adapter, reg)
	res.Checks.FileEdit = probeFileEdit(ctx, cfg, client, adapter, reg, ws)
	res.Checks.JSONOnly = probeJSONOnly(ctx, cfg, client)
	return finishProbeResult(res)
}

func finishProbeResult(res probeResult) probeResult {
	if res.TempWorkspace != "" {
		_ = os.RemoveAll(res.TempWorkspace)
		if _, err := os.Stat(res.TempWorkspace); errors.Is(err, os.ErrNotExist) {
			res.TempWorkspaceClean = true
		}
	}
	res.OK = res.Checks.ToolCall.OK && res.Checks.FileEdit.OK && res.Checks.JSONOnly.OK
	return res
}

func probeToolCall(ctx context.Context, cfg config.Config, client llm.LLMClient, adapter tools.ToolAdapter, reg *tools.Registry) probeCheck {
	call, err := probeOneToolCall(ctx, cfg, client, adapter, reg, "Call the list_dir tool exactly once with path \".\". Do not answer in prose.")
	if err != nil {
		return probeFail(classifyProbeError(err), err)
	}
	if call.Name != tools.NameListDir {
		return probeCheck{FailureCode: "tool_call_invalid", Message: "expected list_dir tool call, got " + call.Name}
	}
	if _, err := reg.Dispatch(ctx, call); err != nil {
		return probeFail(classifyProbeError(err), err)
	}
	return probeCheck{OK: true}
}

func probeFileEdit(ctx context.Context, cfg config.Config, client llm.LLMClient, adapter tools.ToolAdapter, reg *tools.Registry, ws tools.Workspace) probeCheck {
	prompt := "Call edit_file exactly once for probe.txt. Replace the exact line \"before\" with \"after\" using a SEARCH/REPLACE block. Do not answer in prose."
	call, err := probeOneToolCall(ctx, cfg, client, adapter, reg, prompt)
	if err != nil {
		return probeFail(classifyProbeError(err), err)
	}
	if call.Name != tools.NameEditFile && call.Name != tools.NameWriteFile {
		return probeCheck{FailureCode: "tool_call_invalid", Message: "expected edit_file or write_file tool call, got " + call.Name}
	}
	if _, err := reg.Dispatch(ctx, call); err != nil {
		return probeFail(classifyProbeError(err), err)
	}
	content, err := tools.ReadFile(ws, "probe.txt")
	if err != nil {
		return probeFail("edit_failed", err)
	}
	if !strings.Contains(content, "after") || strings.Contains(content, "before") {
		return probeCheck{FailureCode: "edit_failed", Message: "probe.txt did not contain the expected edit"}
	}
	return probeCheck{OK: true}
}

func probeJSONOnly(ctx context.Context, cfg config.Config, client llm.LLMClient) probeCheck {
	resp, err := client.Complete(ctx, llm.ChatRequest{
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Return exactly one JSON object and no prose."},
			{Role: llm.RoleUser, Content: `Return exactly {"ok":true}.`},
		},
	})
	if err != nil {
		return probeFail(classifyProbeError(err), err)
	}
	msg := probeAssistantMessage(resp)
	if err := validateJSONOnly(msg.Content); err != nil {
		return probeFail("json_invalid", err)
	}
	return probeCheck{OK: true}
}

func probeOneToolCall(ctx context.Context, cfg config.Config, client llm.LLMClient, adapter tools.ToolAdapter, reg *tools.Registry, prompt string) (tools.Call, error) {
	req := adapter.BuildRequest(llm.ChatRequest{
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are kloo's capability probe. Follow the user's instruction with exactly one tool call."},
			{Role: llm.RoleUser, Content: prompt},
		},
	}, reg)
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return tools.Call{}, err
	}
	return adapter.Parse(probeAssistantMessage(resp))
}

func probeAssistantMessage(resp llm.ChatResponse) llm.Message {
	if len(resp.Choices) == 0 {
		return llm.Message{Role: llm.RoleAssistant}
	}
	msg := resp.Choices[0].Message
	msg.FinalizeReasoning()
	return msg
}

func probeFail(code string, err error) probeCheck {
	return probeCheck{FailureCode: code, Message: boundedString(err.Error(), 240)}
}

func classifyProbeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, tools.ErrMalformedToolCall), errors.Is(err, tools.ErrNoToolCall), errors.Is(err, tools.ErrMultipleToolCalls), errors.Is(err, tools.ErrUnknownTool), errors.Is(err, tools.ErrInvalidArgs):
		return "tool_call_invalid"
	case strings.Contains(err.Error(), "valid JSON only"):
		return "json_invalid"
	default:
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) || strings.Contains(err.Error(), "llm:") {
			return "model_error"
		}
		return "tool_error"
	}
}

func adapterName(adapter tools.ToolAdapter) string {
	switch adapter.(type) {
	case tools.NativeFCAdapter:
		return "native"
	case tools.XMLAdapter:
		return "xml"
	default:
		return "unknown"
	}
}

func writeProbeJSON(out io.Writer, res probeResult) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(res)
}

func writeProbeHuman(out io.Writer, res probeResult) {
	fmt.Fprintln(out, "kloo probe")
	fmt.Fprintf(out, "model: %s\n", res.Model)
	fmt.Fprintf(out, "endpoint: %s\n", res.Endpoint)
	fmt.Fprintf(out, "tool_format: %s\n", res.ToolFormat)
	writeProbeCheck(out, "tool_call", res.Checks.ToolCall)
	writeProbeCheck(out, "file_edit", res.Checks.FileEdit)
	writeProbeCheck(out, "json_only", res.Checks.JSONOnly)
	if res.TempWorkspace != "" {
		fmt.Fprintf(out, "temp_workspace: %s (removed=%t)\n", res.TempWorkspace, res.TempWorkspaceClean)
	}
	if res.OK {
		fmt.Fprintln(out, "overall: PASS")
	} else {
		fmt.Fprintln(out, "overall: FAIL")
	}
}

func writeProbeCheck(out io.Writer, name string, check probeCheck) {
	if check.OK {
		fmt.Fprintf(out, "%s PASS\n", name)
		return
	}
	fmt.Fprintf(out, "%s FAIL failure_code=%s", name, check.FailureCode)
	if check.Message != "" {
		fmt.Fprintf(out, " message=%q", check.Message)
	}
	fmt.Fprintln(out)
}
