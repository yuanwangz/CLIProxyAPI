package codexquota

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	log "github.com/sirupsen/logrus"
)

// WarmupRoutingHints replays persisted Codex quota snapshots into the
// scheduler's in-memory routing hint map. It is best-effort: any read or
// decode failure is logged and skipped. Intended to be called once during
// service startup, after usagepersist is initialized and before the scheduler
// processes its first request.
func WarmupRoutingHints(ctx context.Context) {
	snapshots, err := usagepersist.QuotaSnapshots(ctx)
	if err != nil {
		log.WithError(err).Debug("codex routing hint warmup: failed to load persisted quota snapshots")
		return
	}
	for _, snapshot := range snapshots {
		if !strings.EqualFold(strings.TrimSpace(snapshot.Provider), providerCodex) {
			continue
		}
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			continue
		}
		raw := strings.TrimSpace(snapshot.QuotaJSON)
		if raw == "" {
			continue
		}
		var state quotaState
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			log.WithError(err).WithField("auth_id", authID).Debug("codex routing hint warmup: invalid quota_json")
			continue
		}
		publishRoutingHintByAuthID(authID, state)
	}
}
