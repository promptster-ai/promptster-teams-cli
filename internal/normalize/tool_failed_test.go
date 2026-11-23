package normalize

import (
	"testing"

	"github.com/promptster-ai/promptster-teams-cli/internal/event"
)

func toolFailed(t *testing.T, events []event.Event) (seen, failed bool) {
	t.Helper()
	for _, e := range events {
		if e.Kind != "file_diff" {
			continue
		}
		seen = true
		d, _ := e.Data.(map[string]interface{})
		failed, _ = d[ToolFailedKey].(bool)
	}
	return seen, failed
}

// A failed or denied Claude edit still reads as a file_diff; it is marked so
// nothing takes the file on disk for its result. A successful one is not.
func TestClaudeEditMarkedFailedOnlyOnError(t *testing.T) {
	for _, c := range []struct {
		isErr bool
	}{{true}, {false}} {
		p := NewClaudeTranscriptProcessor("sess-1")
		res := `{"type":"tool_result","tool_use_id":"toolu_f","content":"ok"}`
		if c.isErr {
			res = `{"type":"tool_result","tool_use_id":"toolu_f","is_error":true,"content":"File has been modified since read"}`
		}
		events := processAll(t, p,
			`{"type":"assistant","message":{"id":"msg-f","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_f","name":"Edit","input":{"file_path":"/tmp/ws/a.go","old_string":"x","new_string":"return nil"}}],"usage":{"input_tokens":10,"output_tokens":5}},"timestamp":"2026-06-10T10:01:00Z"}`,
			`{"type":"user","message":{"role":"user","content":[`+res+`]},"toolUseResult":"r","timestamp":"2026-06-10T10:01:05Z"}`,
		)
		if seen, failed := toolFailed(t, events); !seen || failed != c.isErr {
			t.Errorf("is_error=%v: file_diff seen=%v, %s=%v", c.isErr, seen, ToolFailedKey, failed)
		}
	}
}

func TestCodexPatchMarkedFailedOnlyWhenUnsuccessful(t *testing.T) {
	for _, success := range []string{"false", "true"} {
		p := NewCodexRolloutProcessor("sess-1")
		events := p.Process([]byte(`{"timestamp":"2026-06-06T20:38:50.783Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_A","success":` + success + `,"changes":{"/tmp/ws/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-x\n+return nil\n"}},"status":"completed"}}`))
		if seen, failed := toolFailed(t, events); !seen || failed != (success == "false") {
			t.Errorf("success=%s: file_diff seen=%v, %s=%v", success, seen, ToolFailedKey, failed)
		}
	}
}
