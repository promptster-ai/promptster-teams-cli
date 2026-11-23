package normalize

import (
	"strings"
	"testing"

	"github.com/promptster-ai/promptster-teams-cli/internal/redact"
)

// A kept summary is cut to 100 bytes. A secret straddling that cut must be
// scrubbed whole first: the fragment left after the cut matches no rule at
// egress, and raw records are only pre-scrubbed with stage 1.
func TestKeptPreviewScrubsBeforeTheCut(t *testing.T) {
	secret := "shpat_" + "a1b2c3d4e5f60718293a4b5c6d7e8f90" // Titus-only shape
	if string(redact.RedactBytes([]byte(secret))) == secret {
		t.Skip("titus scanner unavailable")
	}
	desc := strings.Repeat("x", 80) + " " + secret + " then deploy"
	got := keptPreview(desc, 100)
	if strings.Contains(got, "shpat_a1b2") {
		t.Fatalf("a fragment of the secret survived the cut: %q", got)
	}
	if short := keptPreview("short task", 100); short != "short task" {
		t.Fatalf("text that fits must be left for egress untouched: %q", short)
	}
}
