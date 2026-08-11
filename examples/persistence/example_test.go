package persistence_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage/memstore"
)

func Example_sessionJournalAndWorkspaceStores() {
	ctx := context.Background()
	backend := memstore.New()
	sessions, err := sessionstore.Open(backend)
	if err != nil {
		panic(err)
	}
	sessionID, err := uuid.New()
	if err != nil {
		panic(err)
	}
	lease, err := sessions.AcquireLease(ctx, sessionID)
	if err != nil {
		panic(err)
	}
	log, err := sessions.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		panic(err)
	}
	// Opening a journal writes its epoch fence before returning. The product
	// runtime appends commands and events through this single-writer contract.
	_ = log

	workspaces, err := workspacestore.Open(backend.Blobs)
	if err != nil {
		panic(err)
	}
	root, err := os.MkdirTemp("", "harness-workspace-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)
	if err := os.WriteFile(filepath.Join(root, "result.txt"), []byte("durable\n"), 0o600); err != nil {
		panic(err)
	}
	ref, err := workspaces.Snapshot(ctx, root)
	if err != nil {
		panic(err)
	}
	restored, err := os.MkdirTemp("", "harness-restored-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(restored)
	if err := workspaces.Materialize(ctx, ref, restored); err != nil {
		panic(err)
	}
	contents, err := os.ReadFile(filepath.Join(restored, "result.txt"))
	if err != nil {
		panic(err)
	}

	replayer, err := sessions.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		panic(err)
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: sessionID, From: journal.Beginning()})
	if err != nil {
		panic(err)
	}
	defer cursor.Close()
	_, _, replayErr := cursor.Next(ctx)
	fmt.Println(strings.TrimSpace(string(contents)), replayErr == io.EOF, len(ref) > 0)
	if err := lease.Release(ctx); err != nil {
		panic(err)
	}
	// Output:
	// durable true true
}
