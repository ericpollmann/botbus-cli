package console

import (
	"context"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestRenderWelcomeMentionsAgentUserParent(t *testing.T) {
	w := RenderWelcome("myth-compiler", "compiler", "root", &profile.Profile{
		User: "eric", Framing: "prefers this channel over the desktop UI",
	})
	for _, want := range []string{"myth-compiler", "eric", "prefers this channel", "root"} {
		if !strings.Contains(w, want) {
			t.Fatalf("welcome missing %q: %s", want, w)
		}
	}
}

// With an empty Framing the welcome must omit the "who <framing>" clause so the
// sentence reads cleanly (no dangling "who ." or "who ,").
func TestRenderWelcomeOmitsEmptyFraming(t *testing.T) {
	w := RenderWelcome("myth-compiler", "compiler", "root", &profile.Profile{
		User: "eric", Framing: "",
	})
	if strings.Contains(w, "who ") {
		t.Fatalf("empty framing should drop the 'who ...' clause: %s", w)
	}
	// The operator's name must still be present (it's not gated on framing).
	if !strings.Contains(w, "eric") {
		t.Fatalf("welcome should still name the operator: %s", w)
	}
	// And the sentence must not contain the doubled-space / dangling artifact.
	if strings.Contains(w, "  ") {
		t.Fatalf("empty framing produced a doubled space: %q", w)
	}
}

func TestSeedWelcomePublishesChatEnvelope(t *testing.T) {
	fake := hubclient.NewFake()
	if err := SeedWelcome(context.Background(), fake, "inbox-c", "hello agent"); err != nil {
		t.Fatalf("SeedWelcome: %v", err)
	}
	pubs := fake.Published("inbox-c")
	if len(pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(pubs))
	}
	e, _ := envelope.Decode([]byte(strings.TrimPrefix(pubs[0], "botbus: ")))
	if e.Kind != envelope.KindChat || e.From != "botbus" || !strings.Contains(e.Body, "hello agent") {
		t.Fatalf("bad welcome envelope: %+v", e)
	}
}
