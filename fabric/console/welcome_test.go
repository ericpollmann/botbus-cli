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
