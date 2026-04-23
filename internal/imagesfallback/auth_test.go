package imagesfallback

import (
	"encoding/base64"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveWebModel(t *testing.T) {
	freeAuth := authWithPlan("free")
	if got := ResolveWebModel(freeAuth, "gpt-image-2"); got != "auto" {
		t.Fatalf("free gpt-image-2 model = %q, want auto", got)
	}

	paidAuth := authWithPlan("plus")
	if got := ResolveWebModel(paidAuth, "gpt-image-2"); got != "gpt-5-3" {
		t.Fatalf("paid gpt-image-2 model = %q, want gpt-5-3", got)
	}

	if got := ResolveWebModel(nil, "gpt-image-1"); got != "auto" {
		t.Fatalf("gpt-image-1 model = %q, want auto", got)
	}
}

func authWithPlan(plan string) *coreauth.Auth {
	payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_plan_type":%q}}`, plan)
	token := "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	return &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "tester@example.com",
			"access_token": token,
		},
	}
}
