package codexquota

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const providerCodex = "codex"

type rateLimitWindow struct {
	UsedPercent   float64
	WindowMinutes *int64
	ResetAt       *int64
}

type creditsSnapshot struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance,omitempty"`
}

type rateLimitSnapshot struct {
	LimitID   string
	LimitName string
	Primary   *rateLimitWindow
	Secondary *rateLimitWindow
	Credits   *creditsSnapshot
	PlanType  string
}

type quotaState struct {
	Status   string           `json:"status"`
	PlanType string           `json:"planType,omitempty"`
	Windows  []quotaWindow    `json:"windows"`
	Credits  *creditsSnapshot `json:"credits,omitempty"`
	Source   string           `json:"source,omitempty"`
}

type quotaWindow struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	LabelKey      string            `json:"labelKey,omitempty"`
	LabelParams   map[string]string `json:"labelParams,omitempty"`
	UsedPercent   *float64          `json:"usedPercent"`
	ResetLabel    string            `json:"resetLabel"`
	ResetAt       *int64            `json:"resetAt,omitempty"`
	WindowMinutes *int64            `json:"windowMinutes,omitempty"`
}

type rateLimitEvent struct {
	Type             string                 `json:"type"`
	PlanType         string                 `json:"plan_type"`
	RateLimits       *rateLimitEventDetails `json:"rate_limits"`
	Credits          *rateLimitEventCredits `json:"credits"`
	MeteredLimitName string                 `json:"metered_limit_name"`
	LimitName        string                 `json:"limit_name"`
}

type rateLimitEventDetails struct {
	Primary   *rateLimitEventWindow `json:"primary"`
	Secondary *rateLimitEventWindow `json:"secondary"`
}

type rateLimitEventWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes"`
	ResetAt       *int64  `json:"reset_at"`
}

type rateLimitEventCredits struct {
	HasCredits bool    `json:"has_credits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// PersistFromHeaders stores Codex quota data already present on an upstream HTTP response.
func PersistFromHeaders(ctx context.Context, auth *cliproxyauth.Auth, headers http.Header) {
	state, ok := quotaStateFromHeaders(headers)
	if !ok {
		return
	}
	persist(ctx, auth, state)
}

// PersistFromEvent stores quota data from an already-observed codex.rate_limits websocket event.
func PersistFromEvent(ctx context.Context, auth *cliproxyauth.Auth, payload []byte) {
	state, ok := quotaStateFromEvent(payload)
	if !ok {
		return
	}
	persist(ctx, auth, state)
}

// PublishRoutingHintFromSnapshot mirrors a stored Codex quota snapshot into the
// scheduler's in-memory routing hint map. It is intended for management writes
// that already persisted a successful snapshot and should affect routing
// immediately without waiting for a service restart.
func PublishRoutingHintFromSnapshot(provider, authID, quotaJSON string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), providerCodex) {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	raw := strings.TrimSpace(quotaJSON)
	if raw == "" {
		return false
	}
	var state quotaState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		log.WithError(err).WithField("auth_id", authID).Debug("codex routing hint: invalid quota_json")
		return false
	}
	return publishRoutingHintByAuthID(authID, state)
}

func quotaStateFromHeaders(headers http.Header) (quotaState, bool) {
	snapshots := parseAllHeaderRateLimits(headers)
	return quotaStateFromSnapshots(snapshots, "headers")
}

func quotaStateFromEvent(payload []byte) (quotaState, bool) {
	var event rateLimitEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return quotaState{}, false
	}
	if event.Type != "codex.rate_limits" {
		return quotaState{}, false
	}
	limitID := firstNonEmpty(event.MeteredLimitName, event.LimitName, providerCodex)
	snapshot := rateLimitSnapshot{
		LimitID:  normalizeLimitID(limitID),
		PlanType: strings.TrimSpace(event.PlanType),
	}
	if event.RateLimits != nil {
		snapshot.Primary = eventWindowToSnapshot(event.RateLimits.Primary)
		snapshot.Secondary = eventWindowToSnapshot(event.RateLimits.Secondary)
	}
	if event.Credits != nil {
		snapshot.Credits = &creditsSnapshot{
			HasCredits: event.Credits.HasCredits,
			Unlimited:  event.Credits.Unlimited,
			Balance:    trimStringPtr(event.Credits.Balance),
		}
	}
	return quotaStateFromSnapshots([]rateLimitSnapshot{snapshot}, "codex.rate_limits")
}

func parseAllHeaderRateLimits(headers http.Header) []rateLimitSnapshot {
	if headers == nil {
		return nil
	}

	snapshots := []rateLimitSnapshot{parseHeaderRateLimit(headers, "")}
	limitIDs := make(map[string]struct{})
	for name := range headers {
		limitID, ok := headerNameToLimitID(strings.ToLower(name))
		if !ok || limitID == providerCodex {
			continue
		}
		limitIDs[limitID] = struct{}{}
	}
	for limitID := range limitIDs {
		snapshots = append(snapshots, parseHeaderRateLimit(headers, limitID))
	}
	return snapshots
}

func parseHeaderRateLimit(headers http.Header, limitID string) rateLimitSnapshot {
	normalizedLimit := strings.TrimSpace(limitID)
	if normalizedLimit == "" {
		normalizedLimit = providerCodex
	}
	normalizedLimit = strings.ToLower(strings.ReplaceAll(normalizedLimit, "_", "-"))
	prefix := "x-" + normalizedLimit
	return rateLimitSnapshot{
		LimitID:   normalizeLimitID(normalizedLimit),
		LimitName: headerString(headers, prefix+"-limit-name"),
		Primary:   parseHeaderWindow(headers, prefix+"-primary-used-percent", prefix+"-primary-window-minutes", prefix+"-primary-reset-at"),
		Secondary: parseHeaderWindow(headers, prefix+"-secondary-used-percent", prefix+"-secondary-window-minutes", prefix+"-secondary-reset-at"),
		Credits:   parseHeaderCredits(headers),
	}
}

func parseHeaderWindow(headers http.Header, usedHeader, windowHeader, resetHeader string) *rateLimitWindow {
	used, ok := headerFloat(headers, usedHeader)
	if !ok {
		return nil
	}
	windowMinutes := headerIntPtr(headers, windowHeader)
	resetAt := headerIntPtr(headers, resetHeader)
	hasData := used != 0 || (windowMinutes != nil && *windowMinutes != 0) || resetAt != nil
	if !hasData {
		return nil
	}
	return &rateLimitWindow{
		UsedPercent:   used,
		WindowMinutes: windowMinutes,
		ResetAt:       resetAt,
	}
}

func parseHeaderCredits(headers http.Header) *creditsSnapshot {
	hasCredits, okHasCredits := headerBool(headers, "x-codex-credits-has-credits")
	unlimited, okUnlimited := headerBool(headers, "x-codex-credits-unlimited")
	if !okHasCredits || !okUnlimited {
		return nil
	}
	balance := headerString(headers, "x-codex-credits-balance")
	return &creditsSnapshot{
		HasCredits: hasCredits,
		Unlimited:  unlimited,
		Balance:    trimStringPtr(&balance),
	}
}

func eventWindowToSnapshot(window *rateLimitEventWindow) *rateLimitWindow {
	if window == nil {
		return nil
	}
	return &rateLimitWindow{
		UsedPercent:   window.UsedPercent,
		WindowMinutes: window.WindowMinutes,
		ResetAt:       window.ResetAt,
	}
}

func quotaStateFromSnapshots(snapshots []rateLimitSnapshot, source string) (quotaState, bool) {
	state := quotaState{
		Status:  "success",
		Windows: make([]quotaWindow, 0),
		Source:  source,
	}
	for _, snapshot := range snapshots {
		if !hasRateLimitData(snapshot) {
			continue
		}
		if state.PlanType == "" {
			state.PlanType = strings.TrimSpace(snapshot.PlanType)
		}
		if state.Credits == nil && snapshot.Credits != nil {
			state.Credits = snapshot.Credits
		}
		appendWindow := func(kind string, window *rateLimitWindow) {
			if window == nil {
				return
			}
			state.Windows = append(state.Windows, quotaWindowForSnapshot(snapshot, kind, window))
		}
		appendWindow("primary", snapshot.Primary)
		appendWindow("secondary", snapshot.Secondary)
	}
	if len(state.Windows) == 0 {
		return quotaState{}, false
	}
	return state, true
}

func quotaWindowForSnapshot(snapshot rateLimitSnapshot, kind string, window *rateLimitWindow) quotaWindow {
	usedPercent := window.UsedPercent
	limitID := normalizeLimitID(firstNonEmpty(snapshot.LimitID, providerCodex))
	name := firstNonEmpty(snapshot.LimitName, humanizeLimitID(limitID))
	duration := formatWindowDuration(window.WindowMinutes)
	params := map[string]string{
		"duration": duration,
		"name":     name,
	}

	labelKey := "codex_quota.observed_primary_window"
	label := "Primary " + duration
	if kind == "secondary" {
		labelKey = "codex_quota.observed_secondary_window"
		label = "Secondary " + duration
	}
	id := "observed-" + kind
	if limitID != providerCodex {
		labelKey = "codex_quota.observed_additional_primary_window"
		label = name + " primary " + duration
		if kind == "secondary" {
			labelKey = "codex_quota.observed_additional_secondary_window"
			label = name + " secondary " + duration
		}
		id = limitID + "-" + kind
	}

	return quotaWindow{
		ID:            id,
		Label:         label,
		LabelKey:      labelKey,
		LabelParams:   params,
		UsedPercent:   &usedPercent,
		ResetLabel:    formatResetAt(window.ResetAt),
		ResetAt:       window.ResetAt,
		WindowMinutes: window.WindowMinutes,
	}
}

func persist(_ context.Context, auth *cliproxyauth.Auth, state quotaState) {
	if usagepersist.DefaultStore() == nil || auth == nil {
		return
	}
	authIndex := strings.TrimSpace(auth.EnsureIndex())
	if authIndex == "" {
		return
	}
	quotaJSON, err := json.Marshal(state)
	if err != nil {
		log.WithError(err).Debug("failed to encode codex quota snapshot")
		return
	}
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(auth.ID)
	}
	usagepersist.UpsertQuotaSnapshotAsync(usagepersist.QuotaSnapshot{
		Provider:  providerCodex,
		AuthID:    strings.TrimSpace(auth.ID),
		AuthIndex: authIndex,
		FileName:  fileName,
		QuotaJSON: string(quotaJSON),
	})
	publishRoutingHint(auth, state)
}

// publishRoutingHint mirrors the earliest observed reset window into the
// scheduler's provider-neutral routing hint map. Reuses already-parsed
// state; no extra header parsing and no network work.
func publishRoutingHint(auth *cliproxyauth.Auth, state quotaState) bool {
	if auth == nil {
		return false
	}
	return publishRoutingHintByAuthID(strings.TrimSpace(auth.ID), state)
}

func publishRoutingHintByAuthID(authID string, state quotaState) bool {
	if authID == "" {
		return false
	}
	var (
		bestResetUnix int64
		bestWindow    time.Duration
		found         bool
	)
	for _, window := range state.Windows {
		if window.ResetAt == nil || *window.ResetAt <= 0 {
			continue
		}
		if !found || *window.ResetAt < bestResetUnix {
			bestResetUnix = *window.ResetAt
			if window.WindowMinutes != nil && *window.WindowMinutes > 0 {
				bestWindow = time.Duration(*window.WindowMinutes) * time.Minute
			} else {
				bestWindow = 0
			}
			found = true
		}
	}
	if !found {
		return false
	}
	cliproxyauth.SetQuotaRoutingHint(authID, cliproxyauth.QuotaRoutingHint{
		ResetAt: time.Unix(bestResetUnix, 0).UTC(),
		Window:  bestWindow,
	})
	return true
}

func hasRateLimitData(snapshot rateLimitSnapshot) bool {
	return snapshot.Primary != nil || snapshot.Secondary != nil || snapshot.Credits != nil || strings.TrimSpace(snapshot.PlanType) != ""
}

func headerNameToLimitID(name string) (string, bool) {
	const suffix = "-primary-used-percent"
	prefix, ok := strings.CutSuffix(name, suffix)
	if !ok {
		return "", false
	}
	limit, ok := strings.CutPrefix(prefix, "x-")
	if !ok {
		return "", false
	}
	return normalizeLimitID(limit), true
}

func headerString(headers http.Header, name string) string {
	return strings.TrimSpace(headers.Get(name))
}

func headerFloat(headers http.Header, name string) (float64, bool) {
	raw := headerString(headers, name)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, false
	}
	return value, true
}

func headerIntPtr(headers http.Header, name string) *int64 {
	raw := headerString(headers, name)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

func headerBool(headers http.Header, name string) (bool, bool) {
	raw := strings.ToLower(headerString(headers, name))
	switch raw {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func normalizeLimitID(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

func humanizeLimitID(limitID string) string {
	value := strings.TrimSpace(strings.ReplaceAll(limitID, "_", " "))
	if value == "" {
		return providerCodex
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func trimStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func formatWindowDuration(minutes *int64) string {
	if minutes == nil || *minutes <= 0 {
		return "-"
	}
	value := *minutes
	if value%(60*24) == 0 {
		return strconv.FormatInt(value/(60*24), 10) + "d"
	}
	if value%60 == 0 {
		return strconv.FormatInt(value/60, 10) + "h"
	}
	return strconv.FormatInt(value, 10) + "m"
}

func formatResetAt(resetAt *int64) string {
	if resetAt == nil || *resetAt <= 0 {
		return "-"
	}
	return time.Unix(*resetAt, 0).UTC().Format("2006/01/02 15:04 UTC")
}
