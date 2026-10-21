package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// reportUnknownConfigKeys is what a command does with the keys its config
// layers set that are no setting: it records them on the command's event and
// writes a warning for each to stderr. The command runs either way, because a
// key that changes nothing is a reason to look, not to stop.
//
// It runs in resolveWorkspace, where every command acquires its
// workspace once, not inside config.Load, which one command can call several
// times and which commands that read no setting never call.
// [LAW:single-enforcer]
func reportUnknownConfigKeys(ctx context.Context, env environ.Env, workspaceRoot string) {
	unknown, err := config.UnknownKeys(env, pathspec.New(workspaceRoot))
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
