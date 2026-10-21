package store

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A Dolt fetch or push that its context cancels while the remote is not
// answering is ended with its transport, git-remote-http included, and the
// cancel reaches the context's observer through the SQL driver, which keeps
// only an error's message.
func TestCancelledDoltTransferEndsItsTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		transfer func(ctx context.Context, s *Store, remote storage.RemoteName) error
	}{
		{name: "fetch", transfer: func(ctx context.Context, s *Store, remote storage.RemoteName) error {
			return s.SyncFetch(ctx, remote)
		}},
		{name: "push", transfer: func(ctx context.Context, s *Store, remote storage.RemoteName) error {
			_, err := s.SyncPush(ctx, remote, "master", true, false)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			// The accept loop owns every connection it takes, and closes them
			// once the listener closes; the test reads the first.
			accepted := make(chan net.Conn, 1)
			go func() {
				var conns []net.Conn
				defer func() {
					for _, conn := range conns {
						_ = conn.Close()
					}
				}()
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					conns = append(conns, conn)
					select {
					case accepted <- conn:
					default:
					}
				}
			}()

			dir := unrelatedDoltDir(t)
			st, err := Open(context.Background(), testEnv(), dir, "ws")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateIssue(context.Background(), storage.CreateIssueInput{Prefix: "test", Title: "t", Topic: "topic", IssueType: "epic"}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			sync := openSyncOrFatal(t, context.Background(), dir)
			defer sync.Close()
			origin := remoteName(t, "origin")
			if err := sync.SyncAddRemote(context.Background(), origin, "http://"+ln.Addr().String()+"/r.git"); err != nil {
				t.Fatal(err)
			}

			cancels := make(chan *gitcmd.Cancelled, 4)
			ctx, cancel := context.WithCancel(gitcmd.WithObserver(context.Background(), func(c *gitcmd.Cancelled) { cancels <- c }))
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.transfer(ctx, sync, origin) }()
			var conn net.Conn
			select {
			case conn = <-accepted:
			case err := <-done:
				t.Fatalf("the %s returned before reaching the remote: %v", tc.name, err)
			case <-time.After(30 * time.Second):
				t.Fatalf("the %s never connected to the remote", tc.name)
			}

			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("the cancelled %s returned no error", tc.name)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("the %s did not return within 3s of the cancel", tc.name)
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			var netErr net.Error
			if _, err := io.Copy(io.Discard, conn); errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the remote's connection is still open 2s after the cancel: the %s's transport outlived it", tc.name)
			}
			select {
			case c := <-cancels:
				if c.Kill.Processes < 2 || c.Kill.Survivors == nil || *c.Kill.Survivors != 0 {
					t.Fatalf("observed kill = %+v, want git and git-remote-http ended, none surviving", c.Kill)
				}
			default:
				t.Fatalf("the context's observer saw no cancelled git: the cancel did not reach it through the driver")
			}
		})
	}
}
