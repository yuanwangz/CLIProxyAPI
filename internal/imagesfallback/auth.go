package imagesfallback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func IsCodexOAuthAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	accountType, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(accountType), "oauth")
}

func AccessToken(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if value, ok := auth.Metadata["access_token"].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func ResolveWebModel(auth *coreauth.Auth, requestedModel string) string {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		requestedModel = "gpt-image-2"
	}

	switch strings.ToLower(requestedModel) {
	case "gpt-image-1":
		return "auto"
	case "gpt-image-2":
		if isFreePlan(auth) {
			return "auto"
		}
		return "gpt-5-3"
	default:
		return requestedModel
	}
}

func RefreshAccessTokenIfNeeded(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, force bool) (*coreauth.Auth, error) {
	if auth == nil || manager == nil {
		return auth, nil
	}

	needsRefresh := force || strings.TrimSpace(AccessToken(auth)) == ""
	if !needsRefresh {
		if expiresAt, ok := auth.ExpirationTime(); ok && time.Until(expiresAt) <= time.Minute {
			needsRefresh = true
		}
	}
	if !needsRefresh {
		return auth, nil
	}

	executor, ok := manager.Executor(auth.Provider)
	if !ok || executor == nil {
		return auth, nil
	}

	updated, err := executor.Refresh(ctx, auth.Clone())
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return auth, nil
	}
	if _, errUpdate := manager.Update(ctx, updated); errUpdate != nil {
		return updated, nil
	}
	return updated, nil
}

func isFreePlan(auth *coreauth.Auth) bool {
	plan := normalizePlanType(extractPlanType(auth))
	return plan == "" || plan == "free"
}

func extractPlanType(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}

	for _, token := range []string{
		AccessToken(auth),
		metaString(auth, "id_token"),
	} {
		if plan := planTypeFromJWT(token); plan != "" {
			return plan
		}
	}

	for _, key := range []string{
		"chatgpt_plan_type",
		"plan_type",
		"account_type",
		"type",
	} {
		if value := metaString(auth, key); value != "" {
			return value
		}
	}

	return ""
}

func planTypeFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}

	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return ""
		}
	}

	var claims map[string]any
	if err = json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}

	if plan := planTypeFromMap(claims); plan != "" {
		return plan
	}

	if rawAuth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		return planTypeFromMap(rawAuth)
	}

	return ""
}

func planTypeFromMap(values map[string]any) string {
	if len(values) == 0 {
		return ""
	}

	for _, key := range []string{
		"chatgpt_plan_type",
		"plan_type",
		"account_type",
		"type",
	} {
		if raw, ok := values[key]; ok {
			if text, okCast := raw.(string); okCast && strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	return ""
}

func normalizePlanType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "free":
		return "free"
	case "plus", "personal":
		return "plus"
	case "pro", "prolite", "pro_lite":
		return "pro"
	case "team", "business", "enterprise":
		return "team"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func metaString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if value, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
