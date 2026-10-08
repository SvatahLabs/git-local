package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gitlocal/internal/guard"
	"gitlocal/internal/overlay"
)

type jsonContextKey struct{}

type jsonState struct {
	data     any
	warnings []string
}

type jsonError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jsonEnvelope struct {
	SchemaVersion int        `json:"schemaVersion"`
	Command       string     `json:"command"`
	OK            bool       `json:"ok"`
	Data          any        `json:"data,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
	Error         *jsonError `json:"error,omitempty"`
}

func wantsJSON(args []string) bool {
	jsonMode := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			jsonMode = true
		} else if value, ok := strings.CutPrefix(arg, "--json="); ok {
			if parsed, err := strconv.ParseBool(value); err == nil {
				jsonMode = parsed
			} else {
				jsonMode = true
			}
		}
	}
	return jsonMode
}

func jsonEnabled(c *cobra.Command) bool {
	flag := c.Flag("json")
	return flag != nil && flag.Value.String() == "true"
}

func setJSON(c *cobra.Command, data any, warnings ...string) bool {
	if !jsonEnabled(c) {
		return false
	}
	state, ok := c.Context().Value(jsonContextKey{}).(*jsonState)
	if !ok {
		return false
	}
	state.data = data
	state.warnings = append(state.warnings, warnings...)
	return true
}

func writeJSON(out io.Writer, command string, kind string, err error, state *jsonState, captured, diagnostic string) error {
	command = strings.Replace(command, "git-local", "git local", 1)
	if command == "" {
		command = "git local"
	}
	envelope := jsonEnvelope{SchemaVersion: 1, Command: command, OK: err == nil}
	if state != nil {
		envelope.Data = state.data
		envelope.Warnings = append(envelope.Warnings, state.warnings...)
	}
	if err != nil {
		envelope.Error = &jsonError{Code: kind, Message: err.Error()}
	}
	if envelope.Data == nil && captured != "" {
		envelope.Data = struct {
			Text string `json:"text"`
		}{captured}
	}
	if diagnostic != "" {
		for _, line := range strings.Split(strings.TrimSpace(diagnostic), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				envelope.Warnings = append(envelope.Warnings, line)
			}
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(envelope)
}

func newJSONBuffers(mode bool, out, errOut io.Writer) (io.Writer, io.Writer, *bytes.Buffer, *bytes.Buffer) {
	if !mode {
		return out, errOut, nil, nil
	}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return stdout, stderr, stdout, stderr
}

func jsonContext(ctx context.Context, state *jsonState) context.Context {
	return context.WithValue(ctx, jsonContextKey{}, state)
}

func jsonCommand(c *cobra.Command) string {
	if c == nil {
		return "git local"
	}
	return c.CommandPath()
}

func guardFindingsJSON(findings []guard.Finding) []map[string]any {
	result := make([]map[string]any, 0, len(findings))
	for _, finding := range findings {
		result = append(result, map[string]any{
			"path": finding.Path, "workspaceId": finding.WorkspaceID,
			"workspaceRoot": finding.WorkspaceRoot, "commit": finding.Commit,
			"overlayName": finding.OverlayName, "overlayReason": finding.OverlayReason,
			"staging": finding.Staging,
		})
	}
	return result
}

func overlayStatusesJSON(statuses []overlay.Status) []map[string]any {
	result := make([]map[string]any, 0, len(statuses))
	for _, status := range statuses {
		result = append(result, map[string]any{
			"name": status.Name, "path": status.Path, "version": status.Version,
			"working": status.Working, "index": status.Index,
			"upstreamRef": status.UpstreamRef, "upstreamBlob": status.UpstreamBlob,
			"upstream": status.Upstream, "adoptedHunks": status.Adopted, "hunks": status.Hunks,
		})
	}
	return result
}
