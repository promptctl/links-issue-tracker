package workflows

import (
	"encoding/json"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/trace"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// firingTraceKind is this trace kind's directory name under
// <storageDir>/traces/, alongside the sibling "automation" kind.
// [LAW:one-source-of-truth]
const firingTraceKind = "workflows"

// FiredDefinition is one definition that fired on an occasion, and which
// specific values in its declared dimensions matched — the "why" half of a
// firing trace.
type FiredDefinition struct {
	ID      string   `json:"id"`
	Source  string   `json:"source"`
	Path    string   `json:"path"`
	Reasons []string `json:"reasons"`
}

// FiringRecord is one real occasion's Dispatch outcome: the occasion and
// every definition that fired on it. Written only when at least one
// definition fires — see Dispatch.
type FiringRecord struct {
	ID          string            `json:"id"`
	RecordedAt  string            `json:"recorded_at"`
	WorkspaceID string            `json:"workspace_id"`
	Event       string            `json:"event,omitempty"`
	IssueID     string            `json:"issue_id,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Entered     string            `json:"entered,omitempty"`
	Exited      string            `json:"exited,omitempty"`
	Fired       []FiredDefinition `json:"fired"`
}

// recordFiring writes one FiringRecord for an occasion that matched at least
// one definition, reusing the collision-safe writer every trace kind in this
// codebase shares (internal/trace). [LAW:one-source-of-truth]
func recordFiring(ws workspace.Info, o Occasion, matched []Definition) (string, error) {
	fired := make([]FiredDefinition, len(matched))
	for i, def := range matched {
		fired[i] = FiredDefinition{
			ID:      def.ID,
			Source:  string(def.Source),
			Path:    def.Path,
			Reasons: def.MatchReasons(o),
		}
	}
	_, path, err := trace.Write(ws.StorageDir, firingTraceKind, trace.Slug(string(o.Event)), func(id string, recordedAt time.Time) ([]byte, error) {
		record := FiringRecord{
			ID:          id,
			RecordedAt:  recordedAt.Format(time.RFC3339Nano),
			WorkspaceID: ws.WorkspaceID,
			Event:       string(o.Event),
			IssueID:     o.IssueID,
			Labels:      o.Labels,
			Entered:     o.Entered,
			Exited:      o.Exited,
			Fired:       fired,
		}
		payload, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(payload, '\n'), nil
	})
	return path, err
}
