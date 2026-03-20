package app

import (
	"context"
	"fmt"
	"os"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

type App struct {
	Workspace workspace.Info
	Store     *store.Store
}

func Open(ctx context.Context, cwd string) (*App, error) {
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := st.EnsureIssuePrefix(ctx, ws.IssuePrefix); err != nil {
		_ = st.Close()
		return nil, err
	}
	return &App{Workspace: ws, Store: st}, nil
}

func OpenForRead(ctx context.Context, cwd string) (*App, error) {
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenForRead(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return &App{Workspace: ws, Store: st}, nil
}

func OpenFromWD(ctx context.Context) (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get cwd: %w", err)
	}
	return Open(ctx, cwd)
}

func (a *App) Close() error { return a.Store.Close() }
