package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reportConfigLayers is what a command records about its config layers:
// where its project layer came from, and the keys its layers set that are no
// setting, each with a warning on stderr. The command runs either way,
// because a key that changes nothing is a reason to look, not to stop.
//
// It runs where a command acquires its workspace, once, not inside
// config.Load, which one command can call several times and which commands
// that read no setting never call. [LAW:single-enforcer]
func reportConfigLayers(ctx context.Context, env environ.Env, project config.ProjectLayer) {
	telemetry.Set(ctx, "config.project", project.Source())
	unknown, err := config.UnknownKeys(env, project)
	telemetry.Set(ctx, "config.unknown_keys", unknown)
	telemetry.Set(ctx, "config.unknown_keys_error", telemetry.ErrorText(err))
	fmt.Fprint(os.Stderr, unknownConfigKeyWarnings(unknown, err))
}

// unknownConfigKeyWarnings is the line each unknown key adds to the command's
// output, after the line saying which layers' keys could not be checked. The
// message is true whether the key is a misspelling, a removed setting, or one a
// newer lit reads.
func unknownConfigKeyWarnings(unknown []config.UnknownKey, err error) string {
	var b strings.Builder
	if err != nil {
		fmt.Fprintf(&b, "warning: config keys were not checked: %v\n", err)
	}
	for _, key := range unknown {
		fmt.Fprintf(&b, "warning: %s sets %q, which this version of lit does not read, so it has no effect\n", key.File, key.Key)
	}
	return b.String()
}
